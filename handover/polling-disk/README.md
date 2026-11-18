# Polling and disk source handover

PARTIAL handover: PC/WSL-only sources still need final reconciliation. Cloud settlement and seven-file natural-cohort sources HAVE NOW BEEN RECOVERED; see CLOUD-RECOVERY.md.

The initial archive commit8aceac63 retains15 previously pushed source snapshots as additional parents without integrating incompatible changes. Application tree stays at b59eb13e. Use inventory.json to inspect/check out each source separately. DO NOT merge this archival branch into develop: its multi-parent ancestry preserves sources, NOT integration proof.

Additional source patches:
- selection-settlement-c24dc409.patch: exact format-patch of c24dc40940cfe8055debb6da11e18e627122d4b8; original parent c5c52b4c5b2e529b8cb55fff107ee0b7d49465ea. Three files, bounded phase diagnostics only. Older PC two-file draft is superseded.
- ci-natural-cohort-wip.patch: seven-file WIP snapshot against9d7589f525119d2cc2b54bb142b61af77464fe90; default-off diagnostic wiring, not qualified for production.

No product code or deployment changed during handover. No new product tests ran. Earlier CI identity provider race tests passed; cache candidate has sharded test evidence but HOME adoption remained unverified and cleanup disabled. Those results apply only to original candidates. Three cloud-* inventory snapshots are parentless recovered trees, not ordinary patches to cherry-pick wholesale.

Missing: latest PC-only/dirty variants not reconciled while WSL service times out. Do not call ALL work handed over yet. Claude owns all further Controller code/deployment; Dot stopped implementation.
