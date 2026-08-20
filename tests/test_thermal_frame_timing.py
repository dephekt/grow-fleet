# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Bounds the MLX90640 refresh rate from both sides.

The refresh rate looks like an image-quality knob and is actually a data-
integrity one. The Melexis driver splits the 768-word pixel read into 16-word
chunks -- ~52 I2C transactions on a bus shared with the SCD41 and QMP6988 -- so
the read takes tens of milliseconds. If a subpage lands inside that window the
sensor rewrites RAM mid-read, and when the transfer catches the word in flight
the frame comes back with a pixel reading 80C+. One such pixel is enough to
wreck the image, because the palette autoscales to the frame's own min and max.

At 16Hz the subpage period was 62.5ms -- the same order as the read -- and the
tent saw a blue frame every few minutes. Both bounds below are invisible in the
YAML and both fail quietly:

  - too FAST and the sensor overwrites RAM underneath the read;
  - too SLOW and two subpages no longer fit inside one update_interval, so the
    checkerboard halves drift apart in time and every frame is stale.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import _load_device_config, device_spec  # noqa: E402

DEVICE = "atoms3u-sensor-rig"

# Conservative upper estimate of the chunked pixel+aux read, in ms. The device
# now measures this for real (the read_duration sensor); until that lands, the
# figure is the slower of two independent estimates -- one from I2C bus time,
# one from where the corruption appears in the frame.
ASSUMED_READ_MS = 120.0

# How much of the subpage period the read is allowed to occupy. A read that
# fills half its window has no room for bus contention or loop jitter.
MIN_HEADROOM = 2.0

RATE_HZ = {
    "0.5Hz": 0.5,
    "1Hz": 1.0,
    "2Hz": 2.0,
    "4Hz": 4.0,
    "8Hz": 8.0,
    "16Hz": 16.0,
    "32Hz": 32.0,
    "64Hz": 64.0,
}


class ThermalFrameTimingTest(unittest.TestCase):
    def setUp(self) -> None:
        config = _load_device_config(device_spec(DEVICE).config)
        self.mlx = config["mlx90640"]
        rate = str(self.mlx["refresh_rate"])
        self.assertIn(rate, RATE_HZ, f"unknown refresh_rate {rate!r}")
        self.rate = rate
        self.subpage_ms = 1000.0 / RATE_HZ[rate]

    def test_subpage_period_clears_the_chunked_pixel_read(self) -> None:
        """Fast end: the sensor must not rewrite RAM while we are reading it."""
        headroom = self.subpage_ms / ASSUMED_READ_MS
        self.assertGreaterEqual(
            headroom,
            MIN_HEADROOM,
            f"refresh_rate {self.rate} gives a {self.subpage_ms:.1f}ms subpage period "
            f"against a ~{ASSUMED_READ_MS:.0f}ms read ({headroom:.1f}x headroom). "
            f"The sensor will overwrite RAM mid-read and frames will tear.",
        )

    def test_a_whole_frame_still_fits_inside_one_tick(self) -> None:
        """Slow end: both checkerboard halves must refresh within update_interval."""
        frame_ms = 2 * self.subpage_ms
        tick_ms = float(self.mlx["update_interval"])
        self.assertLessEqual(
            frame_ms,
            tick_ms,
            f"refresh_rate {self.rate} needs {frame_ms:.0f}ms for both subpages but "
            f"update_interval is {tick_ms:.0f}ms, so the two halves of every frame "
            f"are captured more than one tick apart.",
        )


if __name__ == "__main__":
    unittest.main()
