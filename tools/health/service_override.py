"""Render a candidate ExecStart-only override; never install or reload a unit."""
from pathlib import PurePosixPath
import re

READER_SHA='987f8f1ad5c77c573b4f0d7de9bff15328085e406deae6246a04fc21e987596c'

def render(bundle):
    if (not isinstance(bundle,str) or bundle.startswith('//') or len(bundle)>2048 or not re.fullmatch(r'/[A-Za-z0-9._/-]+',bundle)
            or str(PurePosixPath(bundle))!=bundle or '..' in PurePosixPath(bundle).parts or bundle=='/'):
        raise ValueError('canonical simple absolute bundle path required')
    # Systemd specifiers, shell quoting, variable expansion and command prefixes
    # are deliberately outside this renderer. The path itself is not custody.
    return ('[Service]\nExecStart=\nExecStart=/usr/bin/python3 -I -S -B '
            +bundle+'/publish_backup_health.py --fcd '+bundle+'/bin/health-reader --sha256 '+READER_SHA+'\n')