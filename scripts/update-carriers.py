#!/usr/bin/env python3
"""Refresh VoCat's offline PLMN name table from Android's carrier database.

The AOSP carrier ID table is maintained for Android's own carrier recognition.
Only unconstrained MCC/MNC records are used here: MVNO matches that also require
an SPN, IMSI prefix, GID or ICCID prefix must not rename the serving MNO.
"""

from __future__ import annotations

import base64
import json
import re
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
FRONTEND_TABLE = ROOT / "web" / "src" / "lib" / "mccmnc.json"
BACKEND_TABLE = ROOT / "internal" / "device" / "mccmnc.json"
SOURCE_URL = (
    "https://android.googlesource.com/platform/packages/providers/"
    "TelephonyProvider/+/master/assets/latest_carrier_id/"
    "carrier_list.textpb?format=TEXT"
)

# PLMNs for which modem firmware and older public tables commonly expose stale
# or blank names. These are kept as small, explicit corrections on top of the
# global AOSP dataset.
MANUAL_CARRIERS = {
    "46000": "China Mobile",
    "46002": "China Mobile",
    "46004": "China Mobile",
    "46007": "China Mobile",
    "46008": "China Mobile",
    "46020": "China Mobile",
    "46001": "China Unicom",
    "46006": "China Unicom",
    "46009": "China Unicom",
    "46010": "China Unicom",
    "46003": "China Telecom",
    "46005": "China Telecom",
    "46011": "China Telecom",
    "46012": "China Telecom",
    "46015": "China Broadnet",
}

# Some territories share an MCC. Keep the PLMN-level ISO assignment where an
# MCC-only fallback cannot distinguish them.
ISO_OVERRIDES = {
    "36251": "an",
    "36269": "cw",
    "36291": "an",
    "64700": "re",
    "64702": "re",
    "64703": "re",
    "64704": "re",
}


def braced_blocks(text: str, marker: str) -> list[str]:
    result: list[str] = []
    offset = 0
    while True:
        start = text.find(marker, offset)
        if start < 0:
            return result
        brace = text.find("{", start + len(marker))
        if brace < 0:
            return result
        depth = 0
        quoted = False
        escaped = False
        for index in range(brace, len(text)):
            char = text[index]
            if quoted:
                if escaped:
                    escaped = False
                elif char == "\\":
                    escaped = True
                elif char == '"':
                    quoted = False
                continue
            if char == '"':
                quoted = True
            elif char == "{":
                depth += 1
            elif char == "}":
                depth -= 1
                if depth == 0:
                    result.append(text[brace + 1 : index])
                    offset = index + 1
                    break
        else:
            raise ValueError(f"unterminated {marker} block")


def textproto_string(block: str, field: str) -> str:
    match = re.search(rf"^\s*{re.escape(field)}:\s*(\"(?:\\.|[^\"\\])*\")", block, re.M)
    return json.loads(match.group(1)) if match else ""


def aosp_carriers(text: str) -> dict[str, str]:
    carriers: dict[str, str] = {}
    for carrier in braced_blocks(text, "carrier_id"):
        name = textproto_string(carrier, "carrier_name").strip()
        if not name:
            continue
        attributes = braced_blocks(carrier, "carrier_attribute")
        for attribute in attributes:
            fields = set(re.findall(r"^\s*([a-zA-Z0-9_]+)\s*:", attribute, re.M))
            if fields - {"mccmnc_tuple"}:
                continue
            for plmn in re.findall(r'^\s*mccmnc_tuple:\s*"(\d{5,6})"', attribute, re.M):
                carriers.setdefault(plmn, name)

        # A few legacy entries put MCC/MNC directly in carrier_id. Remove the
        # nested attributes before checking so constrained MVNO tuples do not
        # leak into the generic map.
        direct = carrier
        for attribute in attributes:
            direct = direct.replace("carrier_attribute {" + attribute + "}", "")
        for plmn in re.findall(r'^\s*mccmnc_tuple:\s*"(\d{5,6})"', direct, re.M):
            carriers.setdefault(plmn, name)
    return carriers


def main() -> None:
    with urllib.request.urlopen(SOURCE_URL, timeout=30) as response:
        source = base64.b64decode(response.read()).decode("utf-8")
    names = aosp_carriers(source)
    table = json.loads(FRONTEND_TABLE.read_text(encoding="utf-8"))
    countries: dict[str, str] = table["i"]
    countries.update({str(mcc): "us" for mcc in range(310, 317)})
    countries.update({"406": "in", "461": "cn"})
    carriers: dict[str, list[str]] = table["c"]
    for plmn, name in names.items():
        previous = carriers.get(plmn)
        iso = previous[1] if previous and len(previous) > 1 else countries.get(plmn[:3], "")
        if plmn[:3] in {str(mcc) for mcc in range(310, 317)}:
            iso = "us"
        iso = ISO_OVERRIDES.get(plmn, iso)
        carriers[plmn] = [name, iso]
    for plmn, name in MANUAL_CARRIERS.items():
        carriers[plmn] = [name, countries.get(plmn[:3], "")]

    version_match = re.search(r"^version:\s*(\d+)", source, re.M)
    output = {
        "c": dict(sorted(carriers.items())),
        "i": dict(sorted(countries.items())),
        "t": sorted(set(table["t"])),
        "meta": {
            "source": "Android Open Source Project carrier_list.textpb",
            "source_url": SOURCE_URL.removesuffix("?format=TEXT"),
            "aosp_version": version_match.group(1) if version_match else "unknown",
            "aosp_generic_records": len(names),
        },
    }
    encoded = json.dumps(output, ensure_ascii=False, separators=(",", ":")) + "\n"
    FRONTEND_TABLE.write_text(encoded, encoding="utf-8", newline="\n")
    BACKEND_TABLE.write_text(encoded, encoding="utf-8", newline="\n")
    print(
        f"updated {len(carriers)} PLMN records "
        f"({len(names)} generic AOSP records, version {output['meta']['aosp_version']})"
    )


if __name__ == "__main__":
    main()
