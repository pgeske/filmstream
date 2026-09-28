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
    # Bandwidth caps in KiB/s (-1 = unlimited); TEASTREAM_MAX_DOWNLOAD_KIB,
    # TEASTREAM_MAX_UPLOAD_KIB and TEASTREAM_BACKGROUND_DOWNLOAD_KIB override
    # them. The totals bound the whole daemon; a torrent nobody is streaming
    # (finishing a snatch, seeding) is also held to background_download_kib so
    # playback keeps the headroom.
    'max_download_kib': -1,
    'max_upload_kib': -1,
    'background_download_kib': -1,
}


# libtorrent download priorities.
PRIORITY_SKIP = 0
PRIORITY_LOW = 1
PRIORITY_NORMAL = 4
PRIORITY_HIGH = 6
PRIORITY_TOP = 7

# The first deadline_bytes of a window (its deadline zone) are fetched in
# reading order: the first missing pieces of the zone, at least URGENT_BYTES
# and MIN_URGENT_PIECES, get piece deadlines and top priority, the rest of the
# zone high priority. libtorrent picks top-priority pieces in random order,
# so a whole zone at top priority spreads the swarm over all of it and the
# piece a reader waits for arrives last as often as first.
DEFAULT_DEADLINE_BYTES = 32 << 20
MIN_DEADLINE_PIECES = 2
URGENT_BYTES = 8 << 20
MIN_URGENT_PIECES = 2
# The first missing piece of a window is due almost immediately; later pieces
# are staggered as if the swarm delivered this many bytes per second.
FIRST_DEADLINE_MS = 100
DEADLINE_PACE_BYTES_PER_SECOND = 16 << 20
MIN_DEADLINE_STEP_MS = 50
MAX_DEADLINE_STEP_MS = 1000

DEFAULT_WINDOW_TTL_MS = 30000
MAX_WINDOW_TTL_MS = 600000
MAX_WAIT_MS = 30000

# While a deadline zone misses a piece the torrent is focused (see
# stream_priorities). A focused torrent keeps at least this much of its
# missing data wanted outside its deadline zones; the plugin re-checks every
# second, and no swarm delivers that much in between.
FOCUS_GUARD_BYTES = 64 << 20
MIN_FOCUS_GUARD_PIECES = 2


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


def urgent_pieces(piece_length):
    return max(MIN_URGENT_PIECES, int(math.ceil(URGENT_BYTES / piece_length)))


def schedule(windows, have, piece_length):
    """Compute (deadlines, zones, boosted) for a torrent's stream windows.

    windows is an iterable of (first, last, deadline_last). Pieces already
    present are skipped. deadlines maps each window's first urgent_pieces
    missing pieces of its deadline zone to milliseconds (the earliest any
    window asks for); zones is every missing deadline zone piece and boosted
    every missing window piece.
    """
    step = deadline_step_ms(piece_length)
    urgent = urgent_pieces(piece_length)
    deadlines = {}
    zones = set()
    boosted = set()
    for first, last, deadline_last in windows:
        rank = 0
        for piece in range(first, last + 1):
            if have[piece]:
                continue
            boosted.add(piece)
            if piece > deadline_last:
                continue
            zones.add(piece)
            if rank < urgent:
                due = FIRST_DEADLINE_MS + rank * step
                if due < deadlines.get(piece, due + 1):
                    deadlines[piece] = due
                rank += 1
    return deadlines, zones, boosted


def stream_priorities(base, windows, have, plan, downloaded, piece_length):
    """Piece priorities for a torrent with stream windows.

    base holds the file-derived priorities, plan is what schedule() returned,
    and downloaded maps each partially downloaded piece to the bytes it
    already has.

    With complete deadline zones, every missing window piece is top priority
    and the rest of the torrent downloads normally. A missing deadline zone
    piece means a stream is (or soon will be) blocked, so the torrent focuses:
    pieces with deadlines are top priority, the rest of the deadline zones
    high, the rest of the windows lowest, and the background is paused.
    Otherwise libtorrent keeps every peer's request queue full of rarest-first
    background blocks, finishes background partial pieces before starting
    stream pieces, and requests no deadline block from a peer with more than
    two seconds of queued requests.

    A torrent with every wanted piece present is finished, and libtorrent then
    disconnects its seeds. So a focused torrent keeps a guard of at least
    FOCUS_GUARD_BYTES of missing data wanted beyond its deadline pieces: the
    rest of the windows, then the pieces following each window. A torrent
    with less than that left is not focused.
    """
    deadlines, zones, boosted = plan
    if zones:
        focused = _focused_priorities(base, windows, have, deadlines, zones, downloaded, piece_length)
        if focused is not None:
            return focused
    priorities = list(base)
    for piece in boosted:
        priorities[piece] = PRIORITY_TOP
    return priorities


def _focused_priorities(base, windows, have, deadlines, zones, downloaded, piece_length):
    priorities = [
        PRIORITY_SKIP if wanted > PRIORITY_SKIP and not have[piece] else wanted
        for piece, wanted in enumerate(base)
    ]
    guard = max(FOCUS_GUARD_BYTES, MIN_FOCUS_GUARD_PIECES * piece_length)

    def want(piece, priority):
        nonlocal guard
        priorities[piece] = priority
        guard -= piece_length - downloaded.get(piece, 0)

    for piece in deadlines:
        priorities[piece] = PRIORITY_TOP
    for piece in zones:
        if piece not in deadlines:
            want(piece, PRIORITY_HIGH)
    ordered = sorted(windows)
    for first, last, _ in ordered:
        for piece in range(first, last + 1):
            if not have[piece] and priorities[piece] == PRIORITY_SKIP:
                want(piece, PRIORITY_LOW)
    for start in [last + 1 for _, last, _ in ordered] + [0]:
        for piece in range(start, len(base)):
            if guard <= 0:
                return priorities
            if base[piece] > PRIORITY_SKIP and not have[piece] and priorities[piece] == PRIORITY_SKIP:
                want(piece, PRIORITY_LOW)
    return priorities if guard <= 0 else None


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
