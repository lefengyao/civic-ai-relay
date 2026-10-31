from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import export_deploy_plugin as export


class ExportPluginTests(unittest.TestCase):
    def test_zip_has_one_installable_directory_with_site_and_panel(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'deploy/civic-plugin').mkdir(parents=True)
            for name, contents in [('deploy/civic-relay.json', '{"schemaVersion":1}'),
                                   ('deploy/civic-plugin/manifest.json', '{"version":"1.0.0"}'),
                                   ('deploy/civic-plugin/panel.js', 'console.log("test")'),
                                   ('deploy/README.md', 'Manual plugin installation')]:
                (root / name).write_text(contents, encoding='utf-8')
            with patch.object(export, 'ROOT', root), patch('sys.argv', ['export_deploy_plugin.py']):
                export.main()
            archives = list((root / 'dist/deploy').glob('civic-relay-plugin-*.zip'))
            self.assertEqual(len(archives), 1)
            with zipfile.ZipFile(archives[0]) as bundle:
                self.assertIsNone(bundle.testzip())
                self.assertEqual(set(bundle.namelist()), {
                    'civic-relay-plugin/manifest.json', 'civic-relay-plugin/panel.js',
                    'civic-relay-plugin/site.json', 'README.zh-CN.md'})
                self.assertEqual(bundle.read('civic-relay-plugin/site.json'),
                                 (root / 'deploy/civic-relay.json').read_bytes())


if __name__ == '__main__':
    unittest.main()
