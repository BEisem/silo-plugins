#!/usr/bin/env python3
"""Build this fork's plugins for linux/amd64 and linux/arm64, then write manifest.json.

    scripts/build.py            # builds into dist/ and rewrites manifest.json
    scripts/build.py --release  # also creates one GitHub release per plugin version

Binaries are release assets, not committed. manifest.json is the catalog index
Silo reads: add its raw URL under Admin > Plugins > Repositories.
Needs Go 1.26+, and gh when --release is used. The release workflow in
.github/workflows runs this on every push to main.

Only the plugins listed in PLUGINS are built and listed in the catalog; the
others in plugins/ are the upstream copies from lockyuk/silo-plugins.
"""
import hashlib, json, os, subprocess, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REPO = os.environ.get("GITHUB_REPOSITORY", "BEisem/silo-plugins")
PLUGINS = os.environ.get("PLUGINS", "filmography").split()
DIST = ROOT / "dist"
ARCHES = ["amd64", "arm64"]


# The server decodes the catalog with encoding/json, which wants enum values as
# numbers, while a plugin's own manifest.json (protojson) uses the names.
ENUMS = {"control": {"ADMIN_FORM_CONTROL_UNSPECIFIED": 0, "ADMIN_FORM_CONTROL_TEXT": 1,
                     "ADMIN_FORM_CONTROL_TEXTAREA": 2, "ADMIN_FORM_CONTROL_PASSWORD": 3,
                     "ADMIN_FORM_CONTROL_NUMBER": 4, "ADMIN_FORM_CONTROL_SWITCH": 5,
                     "ADMIN_FORM_CONTROL_SELECT": 6, "ADMIN_FORM_CONTROL_MULTI_SELECT": 7}}


def numeric_enums(node):
    if isinstance(node, dict):
        return {k: (ENUMS[k][v] if k in ENUMS and isinstance(v, str) else numeric_enums(v)) for k, v in node.items()}
    if isinstance(node, list):
        return [numeric_enums(x) for x in node]
    return node


def sh(*args, cwd=None, env=None):
    subprocess.run(args, cwd=cwd, env=env, check=True)


def main():
    release = "--release" in sys.argv
    DIST.mkdir(exist_ok=True)
    # Releases that already exist are never rebuilt: Go stamps the git revision
    # into a binary, so a rebuild would no longer match the published checksum.
    # Their index entries are carried over unchanged.
    old = {}
    if (ROOT / "manifest.json").exists():
        for e in json.loads((ROOT / "manifest.json").read_text()).get("plugins", []):
            old[(e["manifest"]["plugin_id"], e["manifest"]["version"])] = e
    entries = []
    for name in PLUGINS:
        pdir = ROOT / "plugins" / name
        mpath = pdir / "manifest.json"
        if not mpath.exists():
            continue
        manifest = json.loads(mpath.read_text())
        pid, version = manifest["plugin_id"], manifest["version"]
        tag = f"{pdir.name}-v{version}"
        if (pid, version) in old:
            print(f"{tag}: already released, keeping its index entry")
            entries.append(old[(pid, version)])
            continue
        binaries, assets = {}, []
        for arch in ARCHES:
            out = DIST / f"{pdir.name}-{version}-linux-{arch}"
            env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
            sh("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", f"-s -w -X main.version={version}",
               "-o", str(out), ".", cwd=pdir, env=env)
            digest = hashlib.sha256(out.read_bytes()).hexdigest()
            binaries[f"linux/{arch}"] = {
                "url": f"https://github.com/{REPO}/releases/download/{tag}/{out.name}",
                "checksum": digest,
            }
            assets.append(str(out))
            print(f"{out.name}  {digest}")
        entries.append({"manifest": numeric_enums(manifest), "repo_url": f"https://github.com/{REPO}", "binaries": binaries})
        if release:
            target = ["--target", os.environ["GITHUB_SHA"]] if os.environ.get("GITHUB_SHA") else []
            sh("gh", "release", "create", tag, *assets, "--repo", REPO, *target,
               "--title", f"{pid} {version}", "--notes", f"{pid} {version}")
    (ROOT / "manifest.json").write_text(json.dumps({"plugins": entries}, indent=2) + "\n")
    print("wrote manifest.json with", len(entries), "plugin(s)")


if __name__ == "__main__":
    main()
