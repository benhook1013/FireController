"""Synthetic reviewed-template shape; no real operation bindings."""
TEMPLATE=b"from pathlib import Path\nimport sys,json,hashlib\nR=Path('/root/firecontroller-operations/release-synthetic-template')\nSOURCE='source'\nRELEASE_LEASE=Path('/synthetic/release.lock')\ndef call(*args,**kwargs):raise RuntimeError('synthetic only')\ndef preimage():raise RuntimeError('synthetic only')\ndef window():raise RuntimeError('synthetic only')\nif __name__=='__main__':\n raise RuntimeError('synthetic template only')\n"
OLD_ROOT='/root/firecontroller-operations/release-synthetic-template'
