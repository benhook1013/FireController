"""Bind read-only browser postflight to immutable qualified source, not old literals."""
import hashlib
from html.parser import HTMLParser
import re
import subprocess

class Refuse(ValueError):
    pass

class _Assets(HTMLParser):
    def __init__(self):
        super().__init__()
        self.paths = []
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        value = a.get("src") if tag == "script" else a.get("href") if tag == "link" and a.get("rel") == "stylesheet" else None
        if value is not None:
            if not re.fullmatch(r"/assets/[A-Za-z0-9_.-]+\.(js|css)", value):
                raise Refuse("unexpected executable/style asset path")
            if value in self.paths:
                raise Refuse("duplicate asset")
            self.paths.append(value)

def asset_paths(html):
    parser = _Assets()
    parser.feed(html.decode("utf-8"))
    if not any(p.endswith(".js") for p in parser.paths) or not any(p.endswith(".css") for p in parser.paths):
        raise Refuse("missing executable or style asset")
    return sorted(parser.paths)

def source_binding(checkout, revision):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise Refuse("full immutable source revision required")
    def read(path):
        return subprocess.check_output(["git", "-C", str(checkout), "show", revision + ":internal/webui/assets/" + path])
    html = read("index.html")
    return {"source": revision, "assets": {p: hashlib.sha256(read(p.lstrip("/"))).hexdigest() for p in asset_paths(html)}}

def verify_live(binding, expected_revision, html, fetch):
    # fetch is an existing authorized read-only client; no credential acquisition.
    if binding.get("source") != expected_revision or not re.fullmatch(r"[0-9a-f]{40}", expected_revision):
        raise Refuse("source binding differs")
    paths = asset_paths(html)
    if paths != sorted(binding.get("assets", {})):
        raise Refuse("served asset paths differ from qualified source")
    for path in paths:
        expected = binding["assets"][path]
        if not re.fullmatch(r"[0-9a-f]{64}", expected) or hashlib.sha256(fetch(path)).hexdigest() != expected:
            raise Refuse("served asset bytes differ from qualified source")
    return paths
