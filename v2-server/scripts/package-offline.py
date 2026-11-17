#!/usr/bin/env python3
"""Copy only the five verified prepared resources; never recurse over a user's asset directory."""
import hashlib
import json
import pathlib
import re
import shutil
import sys

FILES = {'node': 'bin/ora-node', 'reaper': 'bin/ora-reaper', 'deno': 'bin/deno',
         'plugin': 'opencode-0.6.11.orax', 'claude': 'claude-0.4.5.orax'}


def pinned_source_commit():
    script = pathlib.Path(__file__).with_name('prepare-node.sh').read_text(encoding='utf-8')
    match = re.search(r'^source_commit=([0-9a-f]{40})$', script, re.MULTILINE)
    if not match:
        raise ValueError('pinned source_commit missing in prepare-node.sh')
    return match.group(1)


def copy_assets(source, destination):
    manifest = json.loads((source / 'assets.json').read_text(encoding='utf-8'))
    if manifest['version'] != 1 or manifest['sourceCommit'] != pinned_source_commit() or manifest['profile'] != 'release':
        raise ValueError('offline packages require pinned release assets')
    # Trust anchors remain in prepare-node.sh; the builder runs it before this copy.
    for key, relative in FILES.items():
        entry = manifest[key]
        original = pathlib.Path(entry['Path'])
        if not original.is_absolute():
            original = source / original
        if (not original.resolve().is_relative_to(source.resolve())
                or original.is_symlink()
                or original.resolve() != (source / relative).resolve()):
            raise ValueError('unexpected asset path: ' + key)
        with original.open('rb') as stream:
            digest = hashlib.file_digest(stream, 'sha256').hexdigest()
        if digest != entry['SHA256']:
            raise ValueError('asset digest mismatch: ' + key)
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(original, target)
        target.chmod(0o755 if relative.startswith('bin/') else 0o644)
        entry['Path'] = relative
    (destination / 'assets.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')


if __name__ == '__main__':
    copy_assets(pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]))
