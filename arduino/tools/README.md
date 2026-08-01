# arduino/tools — third-party OTA tooling

**The files in this directory are NOT covered by the repository's AGPL-3.0
license.** They are vendored from upstream and retain their own terms.

| File | Origin | License |
|---|---|---|
| `lzss.c` | Haruhiko Okumura's LZSS encoder/decoder, as redistributed by Arduino | Public domain (stated in the file header) |
| `lzss.py` | Arduino OTA tooling — `ctypes` wrapper around the compiled `lzss` shared object | Upstream Arduino; no license header present |
| `bin2ota.py` | Arduino OTA tooling — wraps a compiled `.bin` in the OTA header | Upstream Arduino; no license header present |
| `mkota.py` | **Ours.** Stdlib re-implementation of `bin2ota.py` with no `crccheck` dependency | AGPL-3.0-or-later |

`lzss.py` and `bin2ota.py` carry no license header upstream. Arduino's core
repositories are generally LGPL-2.1, but that is not stated in these files, so
their status is treated here as *unresolved* rather than assumed. They are kept
verbatim, unmodified, and attributed.

Only `mkota.py` is original work and carries an SPDX header. It exists precisely
because `bin2ota.py` pulls in a third-party `crccheck` dependency for what
`zlib.crc32` already does — `scripts/build_arduino.py` calls `mkota.py`, not
`bin2ota.py`.

`lzss.so` / `lzss.dylib` are build outputs compiled from `lzss.c` and are not
tracked in git.

## If the licensing matters to you

The only file from this directory on the actual build path is `lzss.py` (for
compression) plus our `mkota.py`. Replacing `lzss.py` with a direct `ctypes`
call, or reimplementing LZSS from the public-domain `lzss.c`, would remove the
unresolved dependency entirely.
