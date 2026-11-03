import importlib.util
import re
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = Path(__file__).with_name("select-go-tests.py")
SPEC = importlib.util.spec_from_file_location("select_go_tests", SCRIPT)
SELECT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SELECT)


class SelectionTests(unittest.TestCase):
    def inventory(self):
        return [SELECT.PREFIX + "internal/" + family for family in SELECT.FAMILIES] + [
            SELECT.PREFIX + name for name in ["cmd/fc", "cmd/fcd", "internal/new_package", "tools/new_tool"]
        ]

    def test_new_packages_and_every_existing_package_have_an_owner(self):
        inventory = "\n".join(reversed(self.inventory())) + "\n"
        isolated = {SELECT.PREFIX + "internal/" + family for family in SELECT.FAMILIES}
        remaining = []
        for shard in SELECT.shards():
            selected = SELECT.select_packages(shard, inventory)
            if shard.startswith("rest-"):
                self.assertTrue(set(selected).isdisjoint(isolated))
                remaining.extend(selected)
            else:
                family = shard.rsplit("-", 1)[0]
                self.assertEqual(selected, [SELECT.PREFIX + "internal/" + family])
        self.assertEqual(len(remaining), len(set(remaining)))
        self.assertEqual(set(remaining) | isolated, set(self.inventory()))

    def test_all_tests_examples_and_fuzz_seeds_belong_to_exactly_one_partition(self):
        names = [f"TestCase{i:03}" for i in range(50)] + ["ExampleAPI", "FuzzDecode"]
        for family, count in SELECT.FAMILIES.items():
            package = SELECT.PREFIX + "internal/" + family
            inventory = "\n".join(reversed(names)) + f"\nBenchmarkIgnore\nok\t{package}\t0.001s\n"
            membership = {name: 0 for name in names}
            for index in range(count):
                expression = SELECT.select_tests(f"{family}-{index}", package, inventory)
                self.assertTrue(expression.startswith("^(") and expression.endswith(")$"))
                self.assertIsNone(re.fullmatch(expression, "TestCase000Suffix"))
                self.assertIsNone(re.fullmatch(expression, "BenchmarkIgnore"))
                for name in names:
                    membership[name] += bool(re.fullmatch(expression, name))
            self.assertEqual(set(membership.values()), {1})

    def test_duplicate_names_share_one_partition_and_order_is_deterministic(self):
        names = [f"TestCase{i:03}" for i in range(50)] + ["ExampleAPI", "FuzzDecode"]
        # Go registers internal and external implementations separately even
        # when their valid top-level names match. One expression selects both.
        repeated = names + [names[0], "ExampleAPI", "FuzzDecode"]
        for family, count in SELECT.FAMILIES.items():
            package = SELECT.PREFIX + "internal/" + family
            baseline = "\n".join(names) + f"\nok\t{package}\t0.001s\n"
            inventory = "\n".join(repeated) + f"\nok\t{package}\t0.001s\n"
            reordered = "\n".join(reversed(repeated)) + f"\nok\t{package}\t0.001s\n"
            expressions = []
            for index in range(count):
                shard = f"{family}-{index}"
                expression = SELECT.select_tests(shard, package, inventory)
                self.assertEqual(expression, SELECT.select_tests(shard, package, baseline))
                self.assertEqual(expression, SELECT.select_tests(shard, package, reordered))
                expressions.append(expression)
            for occurrence in repeated:
                self.assertEqual(sum(bool(re.fullmatch(expr, occurrence)) for expr in expressions), 1)

    def test_bad_package_inventories_and_unknown_shards_fail_closed(self):
        for inventory in ["", "bad/package", "\n".join(self.inventory() * 2), self.inventory()[0] + "\n\n"]:
            with self.subTest(inventory=inventory), self.assertRaises(ValueError):
                SELECT.select_packages("rest-0", inventory)
        for shard in ["rest-2", "backup-8", "reviewexec-6", "unknown-0"]:
            with self.subTest(shard=shard), self.assertRaises(ValueError):
                SELECT.select_packages(shard, "\n".join(self.inventory()))
        with self.assertRaises(ValueError):
            SELECT.select_packages("reviews-0", "\n".join(p for p in self.inventory() if not p.endswith("/reviews")))

    def test_bad_test_inventories_mismatched_packages_and_empty_slices_fail_closed(self):
        package = SELECT.PREFIX + "internal/reviews"
        # Repeating a valid name cannot fill otherwise empty partitions.
        for inventory in ["", "TestOne", "TestOne\nTestOne", "TestOne\nTestOne\nTestTwo\nTestTwo", "TestOne\nPRIVATE unexpected output", "TestOne\n\n"]:
            with self.subTest(inventory=inventory), self.assertRaises(ValueError):
                SELECT.select_tests("reviews-0", package, inventory)
        with self.assertRaises(ValueError):
            SELECT.select_tests("reviews-0", SELECT.PREFIX + "internal/backup", "TestOne")

    def test_real_cli_refuses_invalid_input_without_outputs(self):
        result = subprocess.run([sys.executable, str(SCRIPT), "unknown-0", "not-present"], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")

    def test_workflow_has_every_slice_and_requires_each_independent_job(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        matrix = re.search(r"^        shard: \[([^\]]+)\]$", workflow, re.MULTILINE)
        self.assertIsNotNone(matrix, "Explicit matrix inventory is required")
        actual = [name.strip() for name in matrix[1].split(",")]
        self.assertEqual(len(actual), len(set(actual)))
        self.assertEqual(set(actual), set(SELECT.shards()))
        jobs = set(re.findall(r"^  ([\w-]+):$", workflow.split("\njobs:\n", 1)[1], re.MULTILINE))
        gate = workflow.split("\n  validate:\n", 1)[1]
        needs = re.search(r"^    needs: \[([^\]]+)\]$", gate, re.MULTILINE)
        self.assertIsNotNone(needs)
        required = {name.strip() for name in needs[1].split(",")}
        self.assertEqual(required, jobs - {"validate", "publish"})
        publish = workflow.split("\n  publish:\n", 1)[1]
        self.assertIn("needs: [validate]", publish)
        self.assertIn("github.event_name == 'push'", publish)
        self.assertIn("refs/heads/develop", publish)
        self.assertIn("refs/heads/main", publish)
        self.assertIn("packages: write", publish)
        self.assertNotIn("docker build", publish)
        self.assertIn("sha256sum --check", publish)
        for job in required:
            self.assertIn("needs." + job + ".result", gate)
        self.assertIn("if: ${{ always() }}", gate)
        static = workflow.split("\n  static:\n", 1)[1].split("\n  linux:\n", 1)[0]
        linux = workflow.split("\n  linux:\n", 1)[1].split("\n  qualification:\n", 1)[0]
        self.assertEqual(len(re.findall(r"^        run: make vet$", static, re.MULTILINE)), 1)
        self.assertRegex(linux, r"(?m)^        run: make test-race$")
        self.assertNotIn("vet", linux)
        self.assertIn("run: make test-budgets", workflow)
        self.assertIn("CHANGES_RESULT: ${{ needs.changes.result }}", gate)
        self.assertIn('test "$SOURCE_REF" = refs/heads/develop', gate)
        self.assertIn('test "$EVENT_NAME" = push', gate)
        self.assertIn('test "$LINUX_RESULT" = skipped', gate)
        self.assertIn('test "$NATIVE_RESULT" = skipped', gate)
        makefile = (ROOT / "Makefile").read_text()
        self.assertIn("test: test-race\n\t$(MAKE) test-budgets\n\t$(MAKE) vet", makefile)
        self.assertIn("test -race -count=1", makefile)


if __name__ == "__main__":
    unittest.main()
