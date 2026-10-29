import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("development-checks.py")
SPEC = importlib.util.spec_from_file_location("development_checks", SCRIPT)
SELECT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SELECT)


class DevelopmentChecksTests(unittest.TestCase):
    def test_only_website_paths_can_reuse_backend_checks(self):
        self.assertTrue(SELECT.website_only([
            "webapp/src/styles.css", "internal/webui/assets/index.html", "docs/browser-experience.md",
        ]))
        for paths in [[], [""], ["cmd/fcd/main.go"], ["internal/webui/server.go"],
                      [".github/workflows/ci.yml"], ["Dockerfile"], ["go.mod"],
                      ["webapp/src/styles.css", "internal/reviews/service.go"]]:
            with self.subTest(paths=paths):
                self.assertFalse(SELECT.website_only(paths))

    def test_actual_git_diff_includes_deleted_backend_side_of_a_rename(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(["git", "-C", directory, *args], text=True).strip()

            git("init", "-q")
            git("config", "user.name", "CI fixture")
            git("config", "user.email", "ci@example.invalid")
            root = Path(directory)
            (root / "backend.go").write_text("package backend\n")
            (root / "webapp").mkdir()
            (root / "webapp/style.css").write_text("body{}\n")
            git("add", ".")
            git("commit", "-qm", "qualified")
            base = git("rev-parse", "HEAD")
            (root / "webapp/style.css").write_text("body{color:red}\n")
            git("commit", "-qam", "website")
            website = git("rev-parse", "HEAD")
            result = subprocess.run(["python3", str(SCRIPT), "--base", base, "--head", website], cwd=root, capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout, "website_only=true\n")
            git("mv", "backend.go", "webapp/moved.go")
            git("commit", "-qm", "backend rename")
            head = git("rev-parse", "HEAD")
            for baseline in [base, "", "0" * 40, "bad"]:
                result = subprocess.run(["python3", str(SCRIPT), "--base", baseline, "--head", head], cwd=root, capture_output=True, text=True, check=True)
                self.assertEqual(result.stdout, "website_only=false\n")

    def test_validation_only_accepts_skipped_checks_on_selected_development_push(self):
        workflow = (SCRIPT.parents[2] / ".github/workflows/ci.yml").read_text()
        gate = workflow.split("\n  validate:\n", 1)[1].split("\n  publish:\n", 1)[0]
        script = gate.split("        run: |\n", 1)[1]
        env = dict(CHANGES_RESULT="success", STATIC_RESULT="success", QUALIFICATION_RESULT="success",
                   CONTAINER_RESULT="success", LINUX_RESULT="success", NATIVE_RESULT="success",
                   WEBSITE_ONLY="false", EVENT_NAME="push", SOURCE_REF="refs/heads/main")

        def accepted(**changes):
            return subprocess.run(["bash", "-e", "-c", script], env=env | changes).returncode == 0

        self.assertTrue(accepted())
        self.assertFalse(accepted(LINUX_RESULT="skipped"))
        selected = dict(WEBSITE_ONLY="true", SOURCE_REF="refs/heads/develop", LINUX_RESULT="skipped", NATIVE_RESULT="skipped")
        self.assertTrue(accepted(**selected))
        for changes in [dict(EVENT_NAME="pull_request"), dict(SOURCE_REF="refs/heads/main"),
                        dict(CHANGES_RESULT="failure"), dict(STATIC_RESULT="failure"),
                        dict(QUALIFICATION_RESULT="failure"), dict(CONTAINER_RESULT="failure"),
                        dict(LINUX_RESULT="failure"), dict(NATIVE_RESULT="cancelled")]:
            self.assertFalse(accepted(**(selected | changes)))


if __name__ == "__main__":
    unittest.main()
