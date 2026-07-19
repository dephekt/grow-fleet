/*
 * Hamamatsu C12880MA spectrometer — Arduino UNO R4 WiFi.
 *
 * Reads the 288-pixel spectrum (14-bit, auto-exposed) and publishes it (raw
 * counts) to the site MQTT broker the same way the ESPHome fleet devices do —
 * grow-app ingests it and renders the Spectrum page. All calibration/science
 * lives in grow-app; this firmware stays "dumb".
 *
 * Also a first-class fleet firmware citizen: it publishes a retained
 * _firmware/config (grow-firmware-device.v1) and self-updates over OTA by
 * polling grow-app's firmware manifest (the same endpoint the ESPHome devices'
 * `update: http_request` platform uses). OTA image = LZSS-compressed .ota built
 * by scripts/build_arduino.py and published to ghcr.io/dephekt/grow-fleet-spectrometer.
 *
 * Wiring (C12880MA breakout hatted on the R4): TRG->A0 ST->A1 CLK->A2
 *   VIDEO->A3 (analog) LED->A4  (EOS unconnected).
 * Creds/tokens live in arduino_secrets.h (CI generates it from the fleet secrets).
 */
#include <WiFiS3.h>
#include <ArduinoMqttClient.h>
#include <ArduinoHttpClient.h>
#include <ArduinoJson.h>
#include <OTAUpdate.h>
#include "arduino_secrets.h"

// scripts/build_arduino.py writes fw_version.h (#define FW_VERSION "...") before
// compiling; local builds without it fall back to "dev-local".
#if __has_include("fw_version.h")
#include "fw_version.h"
#endif
#ifndef FW_VERSION
#define FW_VERSION "dev-local"
#endif

// ---- fleet identity (MUST match the published grow-firmware-package.v1 manifest) ----
static const char *nodeId      = "spectrometer";
static const char *projectName = "stackdrift.spectrometer";
static const char *pkgName      = "spectrometer";
static const char *pkgOwner= "stackdrift-firmware";
static const char *chipFamily  = "UNO-R4-WIFI";

static const char *T_SPECTRUM = "grow/daniel-home/spectrometer/spectrum/state";
static const char *T_UI       = "grow/daniel-home/spectrometer/_ui/config";
static const char *T_FW       = "grow/daniel-home/spectrometer/_firmware/config";
// HA-style discovery (registers the device in grow-app) + availability + diagnostic sensor states.
static const char *T_STATUS   = "grow/daniel-home/spectrometer/status";
static const char *T_ST_INTEG = "grow/daniel-home/spectrometer/sensor/integration_time/state";
static const char *T_ST_RSSI  = "grow/daniel-home/spectrometer/sensor/wifi_signal/state";
static const char *T_ST_SAT   = "grow/daniel-home/spectrometer/binary_sensor/saturated/state";

// grow-app (site) — serves the OTA manifest + binary over plain HTTP on the LAN.
static const char *GROW_APP_HOST = "192.168.8.3";
static const int   GROW_APP_PORT = 3080;
static const char *MANIFEST_PATH = "/api/firmware/devices/spectrometer/manifest";

// ---- sensor ----
const uint16_t SPEC_CHANNELS = 288;
const int SPEC_TRG = A0, SPEC_ST = A1, SPEC_CLK = A2, SPEC_VIDEO = A3, WHITE_LED = A4;
const int ADC_BITS = 14;
const uint16_t ADC_FULL = (1u << ADC_BITS) - 1;
uint16_t data[SPEC_CHANNELS];
uint32_t seq = 0;
uint32_t integ_ms = 8;
uint32_t integ_us_measured = 0;   // true per-frame integration time (µs), measured in readSpectrometer()

WiFiClient wifiClient;
MqttClient mqttClient(wifiClient);
// Separate socket for the OTA manifest/binary fetch. Sharing wifiClient would make
// http.get() commandeer the same TCP connection and drop the live MQTT session on
// every firmware check (forcing a reconnect + retained-config re-publish).
WiFiClient httpWifi;

// firmware-check cadence
const unsigned long FW_CHECK_INTERVAL_MS = 3600000UL;   // hourly
unsigned long lastFwCheck = 0;
bool didFirstFwCheck = false;

static inline void clockTick(uint8_t d = 1) {
  digitalWrite(SPEC_CLK, HIGH); delayMicroseconds(d);
  digitalWrite(SPEC_CLK, LOW);  delayMicroseconds(d);
}

void readSpectrometer() {
  const uint8_t d = 1;
  digitalWrite(SPEC_CLK, LOW);  delayMicroseconds(d);
  digitalWrite(SPEC_CLK, HIGH); delayMicroseconds(d);
  digitalWrite(SPEC_CLK, LOW);
  // Integration runs from the ST pulse until readout begins. MEASURE that whole window (the fixed
  // pixel-clocking time PLUS the integ_ms delay), not just the delay: grow-app divides the counts by
  // this to get an absolute per-µs photon rate, so it must be the true exposure. Reporting only the
  // delay makes the rate — and thus PPFD — jump whenever auto-exposure changes integ_ms (worst at
  // integ_ms=0, where the omitted ~170 µs of clocking is the entire exposure but reads as 0).
  // micros() unsigned-subtracts correctly across its ~71 min wrap; our window is always well under it.
  const uint32_t t_int_start = micros();
  digitalWrite(SPEC_ST, HIGH);  delayMicroseconds(d);
  for (int i = 0; i < 15; i++) clockTick(d);
  digitalWrite(SPEC_ST, LOW);
  for (int i = 0; i < 85; i++) clockTick(d);
  if (integ_ms) delay(integ_ms);
  clockTick(d);
  integ_us_measured = micros() - t_int_start;
  for (int i = 0; i < SPEC_CHANNELS; i++) { data[i] = analogRead(SPEC_VIDEO); clockTick(d); }
}

void ensureWifi() {
  if (WiFi.status() == WL_CONNECTED) return;
  Serial.print("WiFi connecting to "); Serial.println(SECRET_SSID);
  WiFi.begin(SECRET_SSID, SECRET_PASS);
  unsigned long t0 = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - t0 < 20000) { delay(400); Serial.print('.'); }
  Serial.println();
  if (WiFi.status() == WL_CONNECTED) { Serial.print("WiFi OK  IP="); Serial.println(WiFi.localIP()); }
}

void publishRetained(const char *topic, const char *payload) {
  mqttClient.beginMessage(topic, (unsigned long)strlen(payload), true, 1);
  mqttClient.print(payload);
  mqttClient.endMessage();
}

void publishUiConfig() {
  publishRetained(T_UI,
    "{\"schema\":\"grow-ui.v1\",\"nodeId\":\"spectrometer\",\"groups\":["
    "{\"id\":\"overview\",\"title\":\"Spectrum\",\"order\":10,\"variant\":\"metrics\","
    "\"surface\":\"dashboard\",\"defaultOpen\":true}],\"entities\":[]}");
}

char fwbuf[512];
void publishFirmwareConfig() {
  snprintf(fwbuf, sizeof(fwbuf),
    "{\"schema\":\"grow-firmware-device.v1\",\"nodeId\":\"%s\",\"projectName\":\"%s\","
    "\"packageOwner\":\"%s\",\"package\":\"%s\",\"device\":\"%s\",\"chipFamily\":\"%s\","
    "\"installedVersion\":\"%s\",\"manifestUrl\":\"http://%s:%d%s\"}",
    nodeId, projectName, pkgOwner, pkgName, nodeId, chipFamily,
    FW_VERSION, GROW_APP_HOST, GROW_APP_PORT, MANIFEST_PATH);
  publishRetained(T_FW, fwbuf);
}

// Shared device block + availability for the discovery configs below. FW_VERSION is a string macro,
// so these concatenate at compile time.
#define SPEC_DEV  "\"device\":{\"identifiers\":[\"spectrometer\"],\"name\":\"Spectrometer\",\"manufacturer\":\"stackdrift\",\"model\":\"C12880MA\",\"sw_version\":\"" FW_VERSION "\"}"
#define SPEC_AVTY "\"availability_topic\":\"grow/daniel-home/spectrometer/status\",\"payload_available\":\"online\",\"payload_not_available\":\"offline\""

// HA-style MQTT discovery so grow-app registers the spectrometer as a real device (Diagnostics +
// Updates tabs, firmware channel control) like the ESPHome fleet. It's a "dumb" sensor, so we
// announce a few genuine diagnostics by hand; retained under the site discovery prefix.
void publishDiscovery() {
  publishRetained("grow/daniel-home/_discovery/sensor/spectrometer/integration_time/config",
    "{\"name\":\"Integration time\",\"unique_id\":\"spectrometer_integration_time\",\"object_id\":\"integration_time\","
    "\"state_topic\":\"grow/daniel-home/spectrometer/sensor/integration_time/state\","
    "\"unit_of_measurement\":\"\xC2\xB5s\",\"state_class\":\"measurement\",\"entity_category\":\"diagnostic\",\"icon\":\"mdi:camera-timer\","
    SPEC_AVTY "," SPEC_DEV "}");
  publishRetained("grow/daniel-home/_discovery/sensor/spectrometer/wifi_signal/config",
    "{\"name\":\"WiFi signal\",\"unique_id\":\"spectrometer_wifi_signal\",\"object_id\":\"wifi_signal\","
    "\"state_topic\":\"grow/daniel-home/spectrometer/sensor/wifi_signal/state\","
    "\"unit_of_measurement\":\"dBm\",\"device_class\":\"signal_strength\",\"state_class\":\"measurement\",\"entity_category\":\"diagnostic\","
    SPEC_AVTY "," SPEC_DEV "}");
  publishRetained("grow/daniel-home/_discovery/binary_sensor/spectrometer/saturated/config",
    "{\"name\":\"Saturated\",\"unique_id\":\"spectrometer_saturated\",\"object_id\":\"saturated\","
    "\"state_topic\":\"grow/daniel-home/spectrometer/binary_sensor/saturated/state\","
    "\"device_class\":\"problem\",\"payload_on\":\"ON\",\"payload_off\":\"OFF\",\"entity_category\":\"diagnostic\","
    SPEC_AVTY "," SPEC_DEV "}");
}

void publishDiagStates(bool sat) {
  char b[24];
  snprintf(b, sizeof(b), "%lu", (unsigned long)integ_us_measured); publishRetained(T_ST_INTEG, b);
  snprintf(b, sizeof(b), "%d", (int)WiFi.RSSI());                   publishRetained(T_ST_RSSI, b);
  publishRetained(T_ST_SAT, sat ? "ON" : "OFF");
}

void ensureMqtt() {
  if (mqttClient.connected()) return;
  mqttClient.setId(nodeId);
  mqttClient.setUsernamePassword(SECRET_MQTT_USER, SECRET_MQTT_PASS);
  // Last will: broker flips us offline (retained) if we drop, so grow-app shows the device offline.
  mqttClient.beginWill(T_STATUS, true, 1);
  mqttClient.print("offline");
  mqttClient.endWill();
  Serial.print("MQTT connecting "); Serial.print(SECRET_MQTT_BROKER); Serial.print(':'); Serial.println(SECRET_MQTT_PORT);
  if (mqttClient.connect(SECRET_MQTT_BROKER, SECRET_MQTT_PORT)) {
    Serial.println("MQTT OK");
    // Discovery FIRST, then the birth (online) message — so grow-app knows the entities and their
    // availability topic before it sees "online" for a fresh install (ESPHome convention). Otherwise
    // the first "online" arrives before any entity references the status topic and the device sticks
    // at Unavailable until it's re-sent.
    publishDiscovery();
    publishUiConfig();
    publishFirmwareConfig();
    publishRetained(T_STATUS, "online");
  } else {
    Serial.print("MQTT FAILED err="); Serial.println(mqttClient.connectError());
    delay(1000);
  }
}

char specbuf[2600];
void publishSpectrum(bool sat) {
  uint32_t integ_us = integ_us_measured;   // true exposure measured in readSpectrometer(), not just the delay
  int n = 0;
  n += snprintf(specbuf + n, sizeof(specbuf) - n,
    "{\"seq\":%lu,\"integration_us\":%lu,\"saturated\":%s,\"adc_bits\":14,\"fw\":\"%s\",\"counts\":[",
    (unsigned long)seq, (unsigned long)integ_us, sat ? "true" : "false", FW_VERSION);
  for (int i = 0; i < SPEC_CHANNELS && n < (int)sizeof(specbuf) - 12; i++)
    n += snprintf(specbuf + n, sizeof(specbuf) - n, i ? ",%u" : "%u", data[i]);
  n += snprintf(specbuf + n, sizeof(specbuf) - n, "]}");
  mqttClient.beginMessage(T_SPECTRUM, (unsigned long)n, true, 1);
  mqttClient.print(specbuf);
  mqttClient.endMessage();
}

// Poll grow-app's firmware manifest; if it advertises a version != ours, pull the
// LZSS-compressed .ota over plain HTTP and self-flash (reboots into the new fw).
void checkForUpdate() {
  if (WiFi.status() != WL_CONNECTED) return;
  HttpClient http(httpWifi, GROW_APP_HOST, GROW_APP_PORT);
  String path = String(MANIFEST_PATH) + "?token=" + SECRET_FIRMWARE_UPDATE_TOKEN;
  http.get(path);
  int status = http.responseStatusCode();
  if (status != 200) { Serial.print("manifest GET "); Serial.println(status); http.stop(); return; }
  String body = http.responseBody();
  http.stop();

  JsonDocument doc;
  if (deserializeJson(doc, body)) { Serial.println("manifest parse failed"); return; }
  const char *latest = doc["version"] | "";
  if (strlen(latest) == 0) return;
  if (strcmp(latest, FW_VERSION) == 0) { Serial.println("firmware up to date"); return; }
  const char *otaPath = doc["builds"][0]["ota"]["path"] | "";
  if (strlen(otaPath) == 0) return;

  String url = String("http://") + GROW_APP_HOST + ":" + GROW_APP_PORT + otaPath;
  Serial.print("OTA "); Serial.print(FW_VERSION); Serial.print(" -> "); Serial.println(latest);
  OTAUpdate ota;
  if (ota.begin("/update.bin") != OTAUpdate::OTA_ERROR_NONE) { Serial.println("ota.begin failed"); return; }
  int sz = ota.download(url.c_str(), "/update.bin");
  if (sz <= 0) { Serial.print("ota.download failed "); Serial.println(sz); return; }
  if (ota.verify() != OTAUpdate::OTA_ERROR_NONE) { Serial.println("ota.verify failed"); return; }
  Serial.println("applying update (reboot)...");
  ota.update("/update.bin");   // does not return on success
}

void setup() {
  pinMode(SPEC_CLK, OUTPUT); pinMode(SPEC_ST, OUTPUT); pinMode(SPEC_TRG, INPUT);
  pinMode(WHITE_LED, OUTPUT); digitalWrite(WHITE_LED, LOW);
  digitalWrite(SPEC_CLK, LOW); digitalWrite(SPEC_ST, LOW);
  analogReadResolution(ADC_BITS);
  Serial.begin(115200);
  delay(300);
  Serial.print("# C12880MA spectrometer  fw="); Serial.println(FW_VERSION);
}

void loop() {
  ensureWifi();
  ensureMqtt();
  mqttClient.poll();

  readSpectrometer();
  uint16_t mn = 65535, mx = 0; int peak = 0;
  for (int i = 0; i < SPEC_CHANNELS; i++) { uint16_t v = data[i]; if (v < mn) mn = v; if (v > mx) { mx = v; peak = i; } }
  bool sat = (mx >= ADC_FULL);
  if (mqttClient.connected()) publishSpectrum(sat);
  if (mqttClient.connected() && seq % 8 == 0) publishDiagStates(sat);  // ~every 6 s

  const uint16_t HI = (uint16_t)((uint32_t)ADC_FULL * 80 / 100);
  const uint16_t LO = (uint16_t)((uint32_t)ADC_FULL * 35 / 100);
  if (sat || mx >= HI)                 { integ_ms = (integ_ms > 1) ? integ_ms / 2 : 0; }
  else if (mx < LO && integ_ms < 500)  { integ_ms = integ_ms ? (integ_ms * 3) / 2 + 1 : 2; }

  // firmware self-update: once ~20 s after boot, then hourly. checkForUpdate() is
  // HTTP-only (it re-checks WiFi itself), so gate on WiFi — not MQTT — and only advance
  // the flag/timestamp when it actually ran, so a slow first connect doesn't burn the
  // first check and stall the next one for an hour.
  if ((!didFirstFwCheck && millis() > 20000) || (millis() - lastFwCheck > FW_CHECK_INTERVAL_MS)) {
    if (WiFi.status() == WL_CONNECTED) {
      didFirstFwCheck = true;
      lastFwCheck = millis();
      checkForUpdate();
    }
  }

  seq++;
  delay(800);
}
