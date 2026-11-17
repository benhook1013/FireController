import copy
import gzip
import hashlib
import io
import tarfile
import unittest

from image_layers import replacement_base, PREFIX, LEGACY_PREFIX, overlay_history


def layer(name='fcd', data=b'daemon', extra=False):
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode='w') as archive:
        entry = tarfile.TarInfo(name); entry.size = len(data); entry.mode = 0o555
        archive.addfile(entry, io.BytesIO(data))
        if extra:
            entry = tarfile.TarInfo('other'); entry.size = 1
            archive.addfile(entry, io.BytesIO(b'x'))
    plain = out.getvalue(); packed = gzip.compress(plain, mtime=0)
    digest = 'sha256:' + hashlib.sha256(packed).hexdigest()
    return {'mediaType': 'application/vnd.oci.image.layer.v1.tar+gzip', 'digest': digest, 'size': len(packed)}, 'sha256:' + hashlib.sha256(plain).hexdigest(), packed


class ReplacementTests(unittest.TestCase):
    def fixture(self, count=3):
        base, diff, packed = layer('base', b'base')
        blobs = {base['digest']: packed}
        mf = {'layers': [base]}; cfg = {'rootfs': {'diff_ids': [diff]}, 'history': [{'created_by': 'base'}], 'config': {'Env': ['keep=1']}}
        for i in range(count):
            desc, diff, packed = layer(data=str(i).encode())
            mf['layers'].append(desc); cfg['rootfs']['diff_ids'].append(diff)
            cfg['history'].append({'created_by': PREFIX + str(i)})
            blobs[desc['digest']] = packed
        return mf, cfg, blobs

    def test_replace_chain_without_changing_input_or_runtime(self):
        mf, cfg, blobs = self.fixture(20)
        original = copy.deepcopy((mf, cfg, blobs))
        result, config, removed = replacement_base(mf, cfg, blobs.__getitem__)
        self.assertEqual(removed, 20)
        self.assertEqual(len(result['layers']), 1)
        self.assertEqual(config['config'], cfg['config'])
        self.assertEqual((mf, cfg, blobs), original)
        self.assertEqual(config['rootfs']['diff_ids'], cfg['rootfs']['diff_ids'][:1])

    def test_unknown_or_empty_history_never_removed(self):
        for history in ({'created_by': 'other'}, {'created_by': PREFIX + 'x', 'empty_layer': True}):
            mf, cfg, blobs = self.fixture(); cfg['history'][-1] = history
            self.assertEqual(replacement_base(mf, cfg, blobs.__getitem__)[2], 0)

    def test_content_and_diff_corruption_refused(self):
        mf, cfg, blobs = self.fixture()
        blobs[mf['layers'][-1]['digest']] += b'x'
        with self.assertRaises(ValueError): replacement_base(mf, cfg, blobs.__getitem__)
        mf, cfg, blobs = self.fixture(); cfg['rootfs']['diff_ids'][-1] = 'sha256:wrong'
        with self.assertRaises(ValueError): replacement_base(mf, cfg, blobs.__getitem__)

    def test_mixed_content_must_not_be_lost(self):
        for name, extra in [('other', False), ('fcd', True)]:
            mf, cfg, blobs = self.fixture()
            desc, diff, packed = layer(name, extra=extra)
            mf['layers'][-1] = desc; cfg['rootfs']['diff_ids'][-1] = diff; blobs[desc['digest']] = packed
            with self.assertRaises(ValueError): replacement_base(mf, cfg, blobs.__getitem__)

    def test_producer_history_round_trip_and_legacy_terminal_chain(self):
        mf, cfg, blobs = self.fixture(3)
        cfg['history'][-1] = overlay_history('a' * 40)
        cfg['history'][-2] = {'created_by': LEGACY_PREFIX + '7a788e57'}
        original = copy.deepcopy((mf, cfg, blobs))
        result, config, removed = replacement_base(mf, cfg, blobs.__getitem__)
        self.assertEqual(removed, 3)
        self.assertEqual(len(result['layers']), 1)
        self.assertEqual(config['config'], cfg['config'])
        self.assertEqual((mf, cfg, blobs), original)
        for revision in ('', '7a788e57', 'A' * 40, 'a' * 39, 'a' * 40 + '\n', None):
            with self.assertRaises(ValueError): overlay_history(revision)

    def test_legacy_marker_never_replaces_unknown_boundary_or_mixed_layer(self):
        mf, cfg, blobs = self.fixture(3)
        cfg['history'][-1] = {'created_by': LEGACY_PREFIX + '7a788e57'}
        cfg['history'][-2] = {'created_by': 'unknown producer'}
        result, config, removed = replacement_base(mf, cfg, blobs.__getitem__)
        self.assertEqual(removed, 1)
        self.assertEqual(result['layers'], mf['layers'][:-1])
        self.assertEqual(config['history'][-1]['created_by'], 'unknown producer')
        desc, diff, packed = layer(extra=True)
        mf['layers'][-1] = desc; cfg['rootfs']['diff_ids'][-1] = diff
        blobs[desc['digest']] = packed
        with self.assertRaises(ValueError): replacement_base(mf, cfg, blobs.__getitem__)

    def test_repeated_producer_replacement_keeps_one_overlay_and_same_final_files(self):
        mf, cfg, blobs = self.fixture(0)
        def files(manifest):
            result = {}
            for desc in manifest['layers']:
                with tarfile.open(fileobj=io.BytesIO(gzip.decompress(blobs[desc['digest']]))) as archive:
                    for member in archive.getmembers():
                        result[member.name] = (member.mode, archive.extractfile(member).read())
            return result
        for i in range(10):
            uncompressed_base = copy.deepcopy(mf)
            mf, cfg, removed = replacement_base(mf, cfg, blobs.__getitem__)
            self.assertEqual(removed, 0 if i == 0 else 1)
            desc, diff, packed = layer(data=('binary-' + str(i)).encode())
            blobs[desc['digest']] = packed
            mf['layers'].append(desc); cfg['rootfs']['diff_ids'].append(diff)
            cfg['history'].append(overlay_history(format(i, '040x')))
            uncompressed_base['layers'].append(desc)
            self.assertEqual(files(mf), files(uncompressed_base))
            self.assertEqual(len(mf['layers']), 2)

    def test_initial_base_with_no_overlays_is_preserved(self):
        mf, cfg, blobs = self.fixture(0)
        result, config, removed = replacement_base(mf, cfg, blobs.__getitem__)
        self.assertEqual((result, config, removed), (mf, cfg, 0))

if __name__ == '__main__': unittest.main()
