"""Fail closed on public PyPI release advisories for every locked platform.

Only public package names and versions are sent to PyPI. Run with Python 3.12+.
There are deliberately no advisory exceptions or blanket severity waivers.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import time
import tomllib
from urllib.request import urlopen


def inspect(package):
    name, version = package
    for attempt in range(3):
        try:
            with urlopen(f"https://pypi.org/pypi/{name}/{version}/json", timeout=20) as response:
                metadata = json.load(response)
            return {
                "name": name,
                "version": version,
                "advisories": [
                    {"id": item["id"], "url": item["link"], "fixed_in": item.get("fixed_in", [])}
                    for item in metadata.get("vulnerabilities", [])
                    if not item.get("withdrawn")
                ],
            }
        except Exception:
            if attempt == 2:
                raise
            time.sleep(attempt + 1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path, default=Path(__file__).with_name("uv.lock"))
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    lock = tomllib.loads(args.lock.read_text())
    packages = sorted({
        (item["name"], item["version"])
        for item in lock["package"]
        if item.get("source", {}).get("registry") == "https://pypi.org/simple"
    })
    with ThreadPoolExecutor(max_workers=6) as pool:
        results = list(pool.map(inspect, packages))
    if args.report:
        args.report.write_text(json.dumps(results, indent=2) + "\n")
    affected = [item for item in results if item["advisories"]]
    for item in affected:
        print(item["name"], item["version"], ", ".join(a["id"] for a in item["advisories"]))
    print(f"Audited {len(results)} locked versions; {len(affected)} have published advisories.")
    raise SystemExit(bool(affected))


if __name__ == "__main__":
    main()
