"""Download hejuapi quickstart docs and rewrite base URLs for local embedding."""

from __future__ import annotations

import json
import re
import time
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT_DIR = ROOT / "web" / "src" / "features" / "docs" / "data"
HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    ),
    "Accept": "application/json",
    "Referer": "https://www.hejuapi.com/docs/quickstart",
}

REPLACEMENTS = [
    ("https://www.hejuapi.com", "https://api.aiimg.lol"),
    ("http://www.hejuapi.com", "https://api.aiimg.lol"),
    ("https://hejuapi.com", "https://api.aiimg.lol"),
    ("http://hejuapi.com", "https://api.aiimg.lol"),
]


def fetch_json(url: str, retries: int = 5) -> dict:
    last_error: Exception | None = None
    for attempt in range(1, retries + 1):
        try:
            req = urllib.request.Request(url, headers=HEADERS)
            with urllib.request.urlopen(req, timeout=60) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except Exception as error:  # noqa: BLE001 - retry transient network failures
            last_error = error
            print(f"retry {attempt}/{retries} for {url}: {error}")
            time.sleep(min(2 * attempt, 8))
    assert last_error is not None
    raise last_error


def rewrite(value):
    if isinstance(value, str):
        for old, new in REPLACEMENTS:
            value = value.replace(old, new)
        return value
    if isinstance(value, list):
        return [rewrite(item) for item in value]
    if isinstance(value, dict):
        return {key: rewrite(item) for key, item in value.items()}
    return value


def collect_pages(nodes: list) -> list[dict]:
    pages: list[dict] = []
    for node in nodes:
        if node.get("type") == "page":
            pages.append(node)
            pages.extend(collect_pages(node.get("children") or []))
        else:
            pages.extend(collect_pages(node.get("children") or []))
    return pages


def main() -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    navigation = fetch_json(
        "https://www.hejuapi.com/api/docs/v2/navigation?locale=zh&space=quickstart"
    )
    if not navigation.get("success"):
        raise SystemExit(f"navigation failed: {navigation}")

    nav_data = rewrite(navigation["data"])
    pages_meta = collect_pages(nav_data)

    pages: dict[str, dict] = {}
    for meta in pages_meta:
        path = meta.get("path") or meta.get("slug")
        slug = meta.get("slug")
        query = urllib.parse.urlencode(
            {"space": "quickstart", "locale": "zh", "path": path}
        )
        url = f"https://www.hejuapi.com/api/docs/v2/pages/{urllib.parse.quote(slug)}?{query}"
        payload = fetch_json(url)
        if not payload.get("success"):
            raise SystemExit(f"page failed {path}: {payload}")
        page = rewrite(payload["data"])
        pages[path] = page
        print(f"fetched {path} ({len(json.dumps(page, ensure_ascii=False))} chars)")

    bundle = {
        "space": {
            "slug": "quickstart",
            "title": "快速开始",
            "description": "Quickstart guides",
        },
        "defaultPath": pages_meta[0]["path"] if pages_meta else "tokenrouter",
        "navigation": nav_data,
        "pages": pages,
    }

    out_file = OUT_DIR / "quickstart.zh.json"
    out_file.write_text(
        json.dumps(bundle, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {out_file} pages={len(pages)}")

    # sanity: no leftover hejuapi host
    text = out_file.read_text(encoding="utf-8")
    leftovers = re.findall(r"hejuapi\.com", text)
    print(f"leftover hejuapi.com mentions: {len(leftovers)}")


if __name__ == "__main__":
    main()
