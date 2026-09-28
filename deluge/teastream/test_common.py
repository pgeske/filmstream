"""Tests for the TeaStream plugin's pure piece math: python3 deluge/teastream/test_common.py"""

import importlib.util
import pathlib
import unittest

# The package __init__ imports Deluge; common.py itself does not.
_spec = importlib.util.spec_from_file_location(
    'teastream_common', pathlib.Path(__file__).parent / 'deluge_teastream' / 'common.py'
)
common = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(common)

MIB = 1 << 20
PIECE = 16 * MIB  # the guard is four pieces


def priorities(windows, have, downloaded=None, base=None):
    base = base if base is not None else [common.PRIORITY_NORMAL] * len(have)
    plan = common.schedule(windows, have, PIECE)
    return common.stream_priorities(base, windows, have, plan, downloaded or {}, PIECE)


def guarded_bytes(result, have, deadlines, downloaded):
    """Missing bytes still wanted outside the deadline pieces."""
    return sum(
        PIECE - downloaded.get(piece, 0)
        for piece, priority in enumerate(result)
        if priority > common.PRIORITY_SKIP and not have[piece] and piece not in deadlines
    )


class StreamPrioritiesTest(unittest.TestCase):
    def test_window_ahead_of_reader_leaves_background_downloading(self):
        have = [False] * 100
        have[10:12] = [True, True]  # the window's deadline pieces are present
        result = priorities([(10, 15, 11)], have)
        self.assertEqual(result[12:16], [common.PRIORITY_TOP] * 4)
        self.assertEqual(result[:10] + result[16:], [common.PRIORITY_NORMAL] * 94)

    def test_missing_deadline_piece_pauses_background(self):
        have = [False] * 100
        have[50] = True  # present background pieces keep their priority
        result = priorities([(10, 17, 13)], have)
        self.assertEqual(result[10:12], [common.PRIORITY_TOP] * 2)
        self.assertEqual(result[12:14], [common.PRIORITY_HIGH] * 2)
        self.assertEqual(result[14:18], [common.PRIORITY_LOW] * 4)
        self.assertEqual(result[50], common.PRIORITY_NORMAL)
        paused = [p for p in range(100) if result[p] == common.PRIORITY_SKIP]
        self.assertEqual(paused, [p for p in range(100) if p < 10 or p > 17 and p != 50])

    def test_focus_keeps_a_guard_so_the_torrent_never_finishes(self):
        # A window made only of deadline pieces (a file head) has no rest to
        # keep wanted: the pieces after it are, up to the guard.
        have = [False] * 100
        downloaded = {2: PIECE - (16 << 10), 3: PIECE // 2}
        result = priorities([(0, 1, 1)], have, downloaded)
        self.assertEqual(result[:2], [common.PRIORITY_TOP] * 2)
        self.assertGreaterEqual(guarded_bytes(result, have, {0, 1}, downloaded), common.FOCUS_GUARD_BYTES)
        self.assertEqual(result[2:8], [common.PRIORITY_LOW] * 6)  # partial pieces count for less
        self.assertEqual(set(result[8:]), {common.PRIORITY_SKIP})

    def test_guard_wraps_to_earlier_pieces_near_the_end(self):
        have = [False] * 100
        result = priorities([(97, 99, 99)], have)
        self.assertEqual(result[97:], [common.PRIORITY_TOP] * 2 + [common.PRIORITY_HIGH])
        self.assertEqual(result[:3], [common.PRIORITY_LOW] * 3)
        self.assertEqual(set(result[3:97]), {common.PRIORITY_SKIP})

    def test_only_the_first_missing_zone_pieces_are_urgent(self):
        # libtorrent picks top-priority pieces in random order, so only the
        # pieces a reader needs next may be top priority.
        have = [False] * 100
        have[10] = have[12] = True
        deadlines, zones, _ = common.schedule([(10, 20, 15)], have, PIECE)
        self.assertEqual(sorted(deadlines), [11, 13])
        self.assertLess(deadlines[11], deadlines[13])
        self.assertEqual(zones, {11, 13, 14, 15})
        result = priorities([(10, 20, 15)], have)
        self.assertEqual([result[p] for p in (11, 13)], [common.PRIORITY_TOP] * 2)
        self.assertEqual(result[14:16], [common.PRIORITY_HIGH] * 2)
        self.assertEqual(result[16:21], [common.PRIORITY_LOW] * 5)

    def test_nearly_complete_torrent_is_not_focused(self):
        have = [True] * 100
        have[10:13] = [False, False, False]
        have[40] = False
        result = priorities([(10, 12, 11)], have)
        self.assertEqual(result[10:13], [common.PRIORITY_TOP] * 3)
        self.assertEqual(result[40], common.PRIORITY_NORMAL)

    def test_unwanted_files_stay_skipped(self):
        have = [False] * 100
        base = [common.PRIORITY_SKIP] * 50 + [common.PRIORITY_NORMAL] * 50
        result = priorities([(60, 61, 61)], have, base=base)
        self.assertEqual(set(result[:50]), {common.PRIORITY_SKIP})
        self.assertEqual(result[62:66], [common.PRIORITY_LOW] * 4)
        self.assertEqual(set(result[66:]), {common.PRIORITY_SKIP})


if __name__ == '__main__':
    unittest.main()
