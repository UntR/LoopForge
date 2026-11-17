import importlib.util
import json
from pathlib import Path
import stat
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('prepare', Path(__file__).with_name('prepare.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
IMAGE = 'ghcr.io/example/fork@sha256:' + 'a' * 64


class PrepareTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def prepare(self, **kw):
        options = dict(output=self.root/'bundle', data_root='/volume1/docker/example/data', image=IMAGE)
        options.update(kw)
        return module.prepare(**options)

    def test_private_bundle_and_remote_execution_boundary(self):
        path = self.prepare()
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((path/'.env').stat().st_mode), 0o600)
        compose = json.loads((path/'compose.yaml').read_text())
        mysql, platform = compose['services']['mysql'], compose['services']['platform']
        self.assertNotIn('ports', mysql)
        self.assertEqual(platform['ports'][0]['host_ip'], '127.0.0.1')
        self.assertNotIn('build', platform)
        self.assertEqual(platform['image'], IMAGE)
        self.assertFalse(any('docker.sock' in str(x) for x in platform['volumes']))
        self.assertEqual(platform['environment']['AGENTFLOW_LOCAL_EXECUTION'], 'false')
        self.assertEqual(platform['environment']['AGENTFLOW_CLOUD_RUNNER_PROJECTS'], '*')
        self.assertEqual(platform['environment']['AGENTFLOW_ALLOW_SIGNUP'], 'false')
        self.assertEqual(platform['environment']['AGENTFLOW_AUTH_REQUIRED'], 'true')
        self.assertIn('not a deployed', (path/'REQUIRES-VALIDATION.txt').read_text())
        env = dict(line.split('=', 1) for line in (path/'.env').read_text().splitlines())
        self.assertEqual(len(set(env.values())), 3)
        self.assertTrue(all(value not in (path/'compose.yaml').read_text() for value in env.values()))

    def test_existing_directory_is_unchanged(self):
        output = self.root/'existing'
        output.mkdir()
        (output/'personal.txt').write_text('keep')
        with self.assertRaises(FileExistsError):
            self.prepare(output=output)
        self.assertEqual((output/'personal.txt').read_text(), 'keep')
        self.assertFalse((output/'.env').exists())

    def test_reject_existing_symlink(self):
        target = self.root/'outside'
        target.mkdir()
        output = self.root/'link'
        output.symlink_to(target, target_is_directory=True)
        with self.assertRaises(FileExistsError):
            self.prepare(output=output)
        self.assertEqual(list(target.iterdir()), [])

    def test_invalid_options_write_nothing(self):
        for kw in [dict(image='latest'), dict(bind='0.0.0.0'), dict(bind='::'), dict(bind='224.0.0.1'), dict(port=1), dict(port=65536), dict(project='bad/name'), dict(data_root='/volume1'), dict(data_root='/'), dict(data_root='relative'), dict(data_root='/volume1/docker/../private'), dict(data_root='/volume1/docker/$PRIVATE')]:
            with self.subTest(kw=kw), self.assertRaises(ValueError):
                self.prepare(**kw)
            self.assertFalse((self.root/'bundle').exists())

    def test_distinct_bundles_do_not_reuse_secrets(self):
        first = self.prepare(output=self.root/'one')
        second = self.prepare(output=self.root/'two')
        self.assertNotEqual((first/'.env').read_text(), (second/'.env').read_text())

    def test_ipv6_specific_binding(self):
        path = self.prepare(bind='::1')
        compose = json.loads((path/'compose.yaml').read_text())
        self.assertEqual(compose['services']['platform']['ports'][0]['host_ip'], '::1')


if __name__ == '__main__':
    unittest.main()
