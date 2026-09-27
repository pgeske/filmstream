#
# TeaStream: streaming control API plugin for Deluge 2.
# SPDX-License-Identifier: GPL-3.0-or-later
#
"""Pure helpers shared by the TeaStream core plugin.

Nothing here touches Deluge or libtorrent so the piece math can be reasoned
about (and exercised) without a running daemon.
"""

import math

DEFAULT_PREFS = {
    # Loopback only: the API controls downloads and exposes tracker URLs.
    'bind': '127.0.0.1',
    'port': 8113,
    # File holding the bearer token. TEASTREAM_TOKEN_FILE overrides it.
    'token_file': '',
    # Root under which each torrent gets its own <root>/<info_hash> directory.
    'save_root': '/downloads',
}

# libtorrent download priorities.
PRIORITY_SKIP = 0
PRIORITY_NORMAL = 4
PRIORITY_TOP = 7

# Pieces within this many bytes of a window's start get deadlines; the rest of
# the window is only raised to top priority. Deadlines make libtorrent request
# a piece from several peers at once, which is what streaming needs right at
# the read position but wasteful for the whole lookahead.
DEFAULT_DEADLINE_BYTES = 32 << 20
MIN_DEADLINE_PIECES = 2
# The first missing piece of a window is due almost immediately; later pieces
# are staggered as if the swarm delivered this many bytes per second.
FIRST_DEADLINE_MS = 100
DEADLINE_PACE_BYTES_PER_SECOND = 16 << 20
MIN_DEADLINE_STEP_MS = 50
MAX_DEADLINE_STEP_MS = 1000

DEFAULT_WINDOW_TTL_MS = 30000
MAX_WINDOW_TTL_MS = 600000
MAX_WAIT_MS = 30000


def file_priorities(num_files, wanted):
    """Return per-file priorities: 'all' wants every file, a list wants only
    those indices and None (or an empty list) wants nothing."""
    if wanted == 'all':
        return [PRIORITY_NORMAL] * num_files
    selected = set(wanted or [])
    return [PRIORITY_NORMAL if i in selected else PRIORITY_SKIP for i in range(num_files)]


def piece_span(offset, size, piece_length):
    """Inclusive piece range covering size bytes at a torrent offset."""
    if size <= 0:
        return None
    return offset // piece_length, (offset + size - 1) // piece_length


def base_piece_priorities(files, priorities, num_pieces, piece_length):
    """Derive piece priorities from file priorities.

    files is a list of (offset, size, is_pad). A piece shared by several files
    takes the highest priority of those files, like libtorrent does.
    """
    base = [PRIORITY_SKIP] * num_pieces
    for index, (offset, size, pad) in enumerate(files):
        priority = priorities[index] if index < len(priorities) else PRIORITY_SKIP
        span = piece_span(offset, size, piece_length)
        if pad or priority <= 0 or span is None:
            continue
        first, last = span
        for piece in range(first, min(last, num_pieces - 1) + 1):
            if base[piece] < priority:
                base[piece] = priority
    return base


def window_pieces(file_offset, file_size, piece_length, num_pieces, offset, length, deadline_bytes):
    """Map a file-relative byte window onto (first, last, deadline_last) pieces.

    Raises ValueError for a window outside the file.
    """
    if offset < 0 or offset >= file_size:
        raise ValueError('offset %d outside file of %d bytes' % (offset, file_size))
    length = max(1, min(length, file_size - offset))
    first, last = piece_span(file_offset + offset, length, piece_length)
    last = min(last, num_pieces - 1)
    deadline_pieces = max(MIN_DEADLINE_PIECES, int(math.ceil(max(deadline_bytes, 0) / piece_length)))
    return first, last, min(last, first + deadline_pieces - 1)


def deadline_step_ms(piece_length):
    step = int(piece_length * 1000 / DEADLINE_PACE_BYTES_PER_SECOND)
    return max(MIN_DEADLINE_STEP_MS, min(MAX_DEADLINE_STEP_MS, step))


def schedule(windows, have, piece_length):
    """Compute (deadlines, boosted) for a torrent's stream windows.

    windows is an iterable of (first, last, deadline_last). Pieces already
    present are skipped. deadlines maps piece -> milliseconds (the earliest any
    window asks for); boosted is every missing piece inside any window.
    """
    step = deadline_step_ms(piece_length)
    deadlines = {}
    boosted = set()
    for first, last, deadline_last in windows:
        rank = 0
        for piece in range(first, last + 1):
            if have[piece]:
                continue
            boosted.add(piece)
            if piece <= deadline_last:
                due = FIRST_DEADLINE_MS + rank * step
                if due < deadlines.get(piece, due + 1):
                    deadlines[piece] = due
                rank += 1
    return deadlines, boosted


def pack_bitfield(have, first, last):
    """Pack have[first..last] into bytes, most significant bit first."""
    count = last - first + 1
    packed = bytearray((count + 7) // 8)
    for i in range(count):
        if have[first + i]:
            packed[i >> 3] |= 0x80 >> (i & 7)
    return bytes(packed)


def build_metainfo(info_section, trackers, bencode):
    """Rebuild a .torrent around the exact info dictionary bytes.

    Splicing the raw info section keeps the info hash identical even when the
    original dictionary is not in canonical bencode form.
    """
    head = {}
    if trackers:
        head['announce'] = trackers[0]
        head['announce-list'] = [[tracker] for tracker in trackers]
    prefix = bencode(head)[:-1] if head else b'd'
    return prefix + b'4:info' + info_section + b'e'
