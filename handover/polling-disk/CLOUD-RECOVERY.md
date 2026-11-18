# Cloud source recovered

The two cloud gaps listed in README were recovered on 2026-11-18. PC/WSL-only gaps remain unresolved.

- selection-settlement-c24dc409.patch: exact git format-patch of clean commit c24dc40940cfe8055debb6da11e18e627122d4b8. Three files. Earlier 35-test run and independent review were reported successful; no tests rerun during handover. Diagnostic phase timing only, emitted after settlement returns when >=10 seconds; not a watchdog or scheduling fix.
- ci-natural-cohort-wip.patch: exact seven-file working-tree snapshot against 9d7589f525119d2cc2b54bb142b61af77464fe90. Five modified tracked files and two untracked source/tests. Default-off one natural cohort identity diagnostic/account; REST still supplies actual CI state. Partial/unqualified integration; prior reservation-focused test evidence is not full wiring or live qualification. No implementation changes made while capturing.

SHA256:
- settlement ea763d5727fb2017efa8494f1c0a35946cc594f2f4ea2bde3dcae620e3a918bb
- cohort 9b6264b5457c14c82071cb6db3629f2d0cc662f9827216a701bf1d5998029088

Apply independently against the specified original source, not blindly on current develop. These patches are archival files only; no live code or deployment was changed.
