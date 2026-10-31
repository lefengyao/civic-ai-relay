import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import package_site_zip as pack


class SitePackageTests(unittest.TestCase):
    def test_collects_docker_context_without_tests_or_private_runtime_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ['Dockerfile.relay', '.dockerignore', 'go.mod', 'go.sum',
                         'cmd/civic-relay/main.go', 'cmd/civic-relay/main_test.go',
                         'internal/config/relay.env', 'internal/config/relay.db',
                         'internal/config/relay.db-wal', 'internal/config/bootstrap-admin-key.txt',
                         'internal/config/key.pem', 'internal/config/key.key',
                         'internal/config/store.go', 'web/admin/app.js']:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('test', encoding='utf-8')
            with patch.object(pack, 'ROOT', directory):
                names = [name for name, _ in pack._collect_files()]
            self.assertIn('.dockerignore', names)
            self.assertIn('internal/config/store.go', names)
            self.assertIn('web/admin/app.js', names)
            for excluded in ['cmd/civic-relay/main_test.go', 'internal/config/relay.env',
                             'internal/config/relay.db', 'internal/config/relay.db-wal',
                             'internal/config/bootstrap-admin-key.txt', 'internal/config/key.pem',
                             'internal/config/key.key']:
                self.assertNotIn(excluded, names)

    def test_real_context_matches_the_site_owned_contract(self):
        root = Path(pack.ROOT)
        document = json.loads((root / 'deploy/civic-relay.json').read_text(encoding='utf-8'))
        names = [name for name, _ in pack._collect_files()]
        for required in document['profile']['package']['requiredFiles']:
            self.assertTrue(any(name.startswith(required) for name in names) if required.endswith('/') else required in names, required)
        self.assertFalse(any(name.endswith('_test.go') for name in names))


if __name__ == '__main__':
    unittest.main()
