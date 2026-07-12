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

WiFiClient wifiClient;
MqttClient mqttClient(wifiClient);

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
  digitalWrite(SPEC_ST, HIGH);  delayMicroseconds(d);
  for (int i = 0; i < 15; i++) clockTick(d);
  digitalWrite(SPEC_ST, LOW);
  for (int i = 0; i < 85; i++) clockTick(d);
  if (integ_ms) delay(integ_ms);
  clockTick(d);
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

void ensureMqtt() {
  if (mqttClient.connected()) return;
  mqttClient.setId(nodeId);
  mqttClient.setUsernamePassword(SECRET_MQTT_USER, SECRET_MQTT_PASS);
  Serial.print("MQTT connecting "); Serial.print(SECRET_MQTT_BROKER); Serial.print(':'); Serial.println(SECRET_MQTT_PORT);
  if (mqttClient.connect(SECRET_MQTT_BROKER, SECRET_MQTT_PORT)) {
    Serial.println("MQTT OK");
    publishUiConfig();
    publishFirmwareConfig();
  } else {
    Serial.print("MQTT FAILED err="); Serial.println(mqttClient.connectError());
    delay(1000);
  }
}

char specbuf[2600];
void publishSpectrum(bool sat) {
  uint32_t integ_us = integ_ms * 1000UL;
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
  HttpClient http(wifiClient, GROW_APP_HOST, GROW_APP_PORT);
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

  const uint16_t HI = (uint16_t)((uint32_t)ADC_FULL * 80 / 100);
  const uint16_t LO = (uint16_t)((uint32_t)ADC_FULL * 35 / 100);
  if (sat || mx >= HI)                 { integ_ms = (integ_ms > 1) ? integ_ms / 2 : 0; }
  else if (mx < LO && integ_ms < 500)  { integ_ms = integ_ms ? (integ_ms * 3) / 2 + 1 : 2; }

  // firmware self-update: once ~20 s after boot, then hourly
  if ((!didFirstFwCheck && millis() > 20000) || (millis() - lastFwCheck > FW_CHECK_INTERVAL_MS)) {
    didFirstFwCheck = true;
    lastFwCheck = millis();
    if (mqttClient.connected()) checkForUpdate();
  }

  seq++;
  delay(800);
}
