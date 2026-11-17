"""Keep build-tag-only integration proof in the existing qualification gate."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]


class CohortIntegrationGateTests(unittest.TestCase):
    def test_selection_is_exact_complete_and_nonempty(self):
        names = []
        for path in (ROOT / 'internal/reviewexec').glob('*_test.go'):
            source = path.read_text()
            if source.startswith('//go:build cohortintegration\n'):
                names.extend(re.findall(r'^func (Test\w+)\(', source, re.M))
        self.assertTrue(names)
        self.assertEqual(len(names), len(set(names)))
        make = (ROOT / 'Makefile').read_text()
        selected = re.search(r'^COHORT_INTEGRATION_TESTS := (.+)$', make, re.M)
        self.assertIsNotNone(selected)
        self.assertEqual(selected[1].split('|'), sorted(names))
        self.assertIn("\t$(GO) test -v -race -tags=cohortintegration -count=1 -p=2 -timeout=5m -run '^($(COHORT_INTEGRATION_TESTS))$$' ./internal/reviewexec\n", make)

    def test_existing_qualification_job_runs_mandatory_gate(self):
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        qualification = workflow.split('\n  qualification:\n', 1)[1].split('\n  container:\n', 1)[0]
        expected = ('      - name: Check tagged provider-publication integration\n'
                    '        timeout-minutes: 10\n'
                    '        run: make test-cohort-integration\n')
        self.assertIn(expected, qualification)
        self.assertEqual(workflow.count('run: make test-cohort-integration'), 1)
        self.assertNotIn('continue-on-error:', qualification)


if __name__ == '__main__':
    unittest.main()
