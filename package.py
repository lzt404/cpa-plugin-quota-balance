#!/usr/bin/env python3
"""Package the built shared library into the plugin store asset layout."""

import pathlib
import sys
import zipfile

PLUGIN_ID = "quota-balance"
MEMBERS = (
    f"{PLUGIN_ID}.so",
    "manifest.json",
    "README.md",
    "README.zh-CN.md",
    "LICENSE",
)


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: package.py <staged-dir> <output-zip>", file=sys.stderr)
        return 2
    stage = pathlib.Path(sys.argv[1])
    target = pathlib.Path(sys.argv[2])
    for name in MEMBERS:
        if not (stage / name).is_file():
            print(f"missing release member: {stage / name}", file=sys.stderr)
            return 1
    target.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as archive:
        for name in MEMBERS:
            archive.write(stage / name, name)
    print(target)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
