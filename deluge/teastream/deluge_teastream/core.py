#
# TeaStream: streaming control API plugin for Deluge 2.
# SPDX-License-Identifier: GPL-3.0-or-later
#
"""TeaStream core plugin: a small loopback HTTP JSON API for streaming.

Filmstream drives Deluge through this API instead of embedding its own
BitTorrent engine. Everything goes through Deluge's own components (Core,
TorrentManager, Torrent.handle); Deluge itself is not modified.

Enabling the plugin also turns off DHT, PEX (ut_pex), LSD, UPnP and NAT-PMP in
Deluge's core config, as private trackers require. libtorrent additionally
honours the BEP27 private flag per torrent. Deluge's peer id and user agent are
left untouched. It also tunes three libtorrent session settings for streaming
(see STREAMING_SESSION_SETTINGS).

Configuration (``teastream.conf`` in Deluge's config dir; environment wins):
``bind`` / TEASTREAM_BIND (default 127.0.0.1), ``port`` / TEASTREAM_PORT
(default 8113), ``token_file`` / TEASTREAM_TOKEN_FILE, ``save_root`` /
TEASTREAM_SAVE_ROOT (default /downloads).

Authentication: every endpoint except ``GET /v1/health`` needs
``Authorization: Bearer <token>``, the stripped contents of the token file. With
no readable token file those endpoints answer 503. Responses are JSON; errors
are ``{"error": "..."}`` with a 4xx/5xx status. ``{hash}`` is the 40-character
lowercase hex info hash (Deluge's torrent id).

GET /v1/health
    {"ok", "deluge", "libtorrent", "listen_port", "dht", "lsd", "upnp",
    "natpmp", "utpex", "pex_extension_loaded", "token_configured"}.
    pex_extension_loaded is true when ut_pex was enabled as deluged started; it
    stays loaded until deluged restarts.

POST /v1/torrents
    {"torrent": "<base64 .torrent>"} or {"magnet": "<uri>"}, plus optional
    "save_root" (absolute; data goes to <save_root>/<hash>) and "wanted_files":
    null (download nothing yet), "all", or a list of file indices (others get
    priority 0). Magnet selections apply when metadata arrives. Torrents are
    added started, not auto-managed (Deluge's queue never pauses them), without
    sequential download or move-on-complete. Idempotent:
    -> {"info_hash", "added": false, "save_path"} when already present,
    otherwise {"info_hash", "added": true, "save_path"}.

GET /v1/torrents
    {"torrents": [summary]}. A summary is {"info_hash", "name", "state",
    "has_metadata", "private", "total_size", "total_done", "total_wanted",
    "total_wanted_done", "progress" (whole torrent 0..1), "download_rate",
    "upload_rate" (payload bytes/s), "num_peers", "num_seeds" (connected),
    "list_peers", "list_seeds" (known), "connect_candidates",
    "all_time_download", "all_time_upload", "seeding_seconds" (time seeding
    while complete), "finished_seconds" (time with all wanted pieces),
    "active_seconds", "is_finished", "is_seeding", "paused", "save_path",
    "tracker_status", "tracker_message" (last tracker error/warning or ""),
    "error", "file_progress" (completed bytes per file, piece granularity)}.

GET /v1/torrents/{hash}
    summary plus "piece_length", "num_pieces", "trackers" and "files":
    [{"index", "path" (relative to save_path, "/"-separated), "size",
    "offset" (in the torrent), "pad", "priority", "done"}].

DELETE /v1/torrents/{hash}?remove_data=1
    Removes the torrent (and its data when remove_data=1). -> {"removed": true}

GET /v1/torrents/{hash}/pieces?first=A&last=B
    Piece bitfield for pieces A..B (defaults: the whole torrent).
    -> {"first", "last", "bitfield" (base64, most significant bit first),
    "complete" (true when every piece in the range is present)}.

POST /v1/torrents/{hash}/wait
    {"piece": P, "through": Q, "timeout_ms": T}. Long-poll: answers as soon as
    piece P is present or T (max 30000) elapses, with the pieces response for
    P..Q where "complete" reports piece P only.

PUT /v1/torrents/{hash}/files
    {"wanted": "all" | [indices]} sets file priorities through Deluge (normal
    for wanted, skip for the rest) so they persist. When a skipped file
    becomes wanted, the answer waits (up to 10 s) for libtorrent to move its
    pieces out of the part file, so they are readable from disk once it
    returns. -> {"priorities": [...]}

PUT /v1/torrents/{hash}/windows/{stream}
    {"file", "offset", "length", "deadline_bytes" (optional, default 32 MiB),
    "ttl_ms" (optional, default 30000, max 600000)}. Replaces the stream's
    window: its missing pieces get top priority, and its first deadline_bytes
    (the deadline zone) are fetched in reading order, the first missing ones
    with staggered piece deadlines. Pieces that left the window lose their
    deadline. Windows of all streams of a torrent are merged, so several
    readers (probe, packager, subtitles) can stream concurrently; the rest of
    the wanted files keep normal priority and finish in the background. While
    a deadline zone misses a piece, the torrent focuses on its streams
    instead: the background pauses and only the pieces due next keep top
    priority (see common.stream_priorities). A window expires after ttl_ms
    unless it is refreshed.
    -> {"first_piece", "last_piece", "deadline_last_piece"}

DELETE /v1/torrents/{hash}/windows/{stream}
    -> {"removed": bool}

GET /v1/torrents/{hash}/metainfo
    -> {"torrent": "<base64 .torrent>"} (Deluge's saved copy, or rebuilt
    around the exact info dictionary for magnets).

PUT /v1/listen-port
    {"port": N} -> {"port": N}. Sets a fixed incoming port (the VPN's forwarded
    port can change on reconnect).
"""

import base64
import hmac
import json
import logging
import os
import time

from twisted.internet import reactor, task
from twisted.web import resource, server

import deluge.component as component
from deluge._libtorrent import lt
from deluge.common import get_magnet_info, get_version
from deluge.configmanager import ConfigManager, get_config_dir
from deluge.plugins.pluginbase import CorePluginBase

from .common import (
    DEFAULT_DEADLINE_BYTES,
    DEFAULT_PREFS,
    DEFAULT_WINDOW_TTL_MS,
    MAX_WAIT_MS,
    MAX_WINDOW_TTL_MS,
    PRIORITY_SKIP,
    build_metainfo,
    base_piece_priorities,
    file_priorities,
    pack_bitfield,
    schedule,
    stream_priorities,
    window_pieces,
)

log = logging.getLogger(__name__)

PRIVACY_SETTINGS = {'dht': False, 'lsd': False, 'upnp': False, 'natpmp': False, 'utpex': False}
# Streaming needs piece priorities from the first piece and web seeds that can
# turn to a new read position quickly. libtorrent picks random pieces, ignoring
# priorities and so every stream window, until a torrent has
# initial_picker_threshold pieces; and a web seed fetches whole
# urlseed_max_request_bytes runs (16 MiB by default), which a time-critical
# piece must wait behind. libtorrent also caches failed tracker DNS lookups
# for resolver_cache_timeout (20 minutes by default): deluged usually starts
# before the VPN's resolver answers, and every announce to those trackers,
# including a new playback's, would fail from that cache until it expired.
STREAMING_SESSION_SETTINGS = {
    'initial_picker_threshold': 0,
    'urlseed_max_request_bytes': 2 << 20,
    'resolver_cache_timeout': 60,
}
WAIT_POLL_SECONDS = 0.05
# Upper bound on waiting for libtorrent to confirm new file priorities.
FILE_PRIORITY_TIMEOUT_SECONDS = 10


class ApiError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


class Window:
    __slots__ = ('first', 'last', 'deadline_last', 'expires')

    def __init__(self, first, last, deadline_last, expires):
        self.first = first
        self.last = last
        self.deadline_last = deadline_last
        self.expires = expires


class Streams:
    """Stream windows of one torrent and the deadlines currently applied."""

    def __init__(self):
        self.windows = {}
        self.deadlines = set()
        self.boosted = False


class Waiter:
    __slots__ = ('request', 'info_hash', 'piece', 'through', 'expires', 'done')

    def __init__(self, request, info_hash, piece, through, expires):
        self.request = request
        self.info_hash = info_hash
        self.piece = piece
        self.through = through
        self.expires = expires
        self.done = False


class FilePriorityWaiter:
    """A set-files request answered once libtorrent applied the priorities."""

    __slots__ = ('request', 'payload', 'done')

    def __init__(self, request, payload):
        self.request = request
        self.payload = payload
        self.done = False


class Core(CorePluginBase):
    def enable(self):
        self.config = ConfigManager('teastream.conf', DEFAULT_PREFS)
        self.core = component.get('Core')
        self.torrents = component.get('TorrentManager')
        self.streams = {}
        self.pending_selection = {}
        self.waiters = []
        self.file_priority_waiters = {}
        self.waiter_loop = task.LoopingCall(self._poll_waiters)
        self._token_cache = (None, None)

        # ut_pex is loaded while preferences start when utpex is on, before
        # plugins are enabled, and libtorrent cannot unload an extension.
        self.pex_extension_loaded = bool(self.core.config['utpex'])
        self.core.set_config(dict(PRIVACY_SETTINGS))
        self.core.apply_session_settings(dict(STREAMING_SESSION_SETTINGS))
        if self.pex_extension_loaded:
            log.warning(
                'TeaStream disabled PEX, but ut_pex was loaded when deluged started; '
                'restart deluged to unload it (private torrents never use PEX).'
            )

        alerts = component.get('AlertManager')
        alerts.register_handler('metadata_received', self._on_metadata_received)
        alerts.register_handler('file_prio', self._on_file_priorities_applied)
        bind = os.environ.get('TEASTREAM_BIND') or self.config['bind']
        port = int(os.environ.get('TEASTREAM_PORT') or self.config['port'])
        site = _QuietSite(_Api(self))
        self.listener = reactor.listenTCP(port, site, interface=bind)
        log.info('TeaStream API listening on %s:%d', bind, port)

    def disable(self):
        alerts = component.get('AlertManager')
        alerts.deregister_handler(self._on_metadata_received)
        alerts.deregister_handler(self._on_file_priorities_applied)
        if self.waiter_loop.running:
            self.waiter_loop.stop()
        for waiter in list(self.waiters) + [w for ws in self.file_priority_waiters.values() for w in ws]:
            self._finish(waiter, 503, {'error': 'TeaStream plugin disabled'})
        self.waiters = []
        self.file_priority_waiters = {}
        for info_hash, streams in list(self.streams.items()):
            streams.windows.clear()
            self._apply(info_hash)
        self.streams = {}
        return self.listener.stopListening()

    def update(self):
        now = time.monotonic()
        for info_hash, streams in list(self.streams.items()):
            for stream, window in list(streams.windows.items()):
                if window.expires <= now:
                    del streams.windows[stream]
            # Refreshing keeps deadlines relative to now and restores the boost
            # if something (a Deluge file-priority change) reset priorities.
            self._apply(info_hash)

    # --- request helpers ---

    def authorize(self, request):
        token = self._token()
        if not token:
            raise ApiError(503, 'TeaStream token is not configured')
        header = (request.getHeader('authorization') or '').strip()
        scheme, _, supplied = header.partition(' ')
        if scheme.lower() != 'bearer' or not hmac.compare_digest(
            supplied.strip().encode(), token.encode()
        ):
            raise ApiError(401, 'invalid or missing bearer token')

    def _token(self):
        path = os.environ.get('TEASTREAM_TOKEN_FILE') or self.config['token_file']
        if not path:
            return None
        try:
            mtime = os.stat(path).st_mtime_ns
        except OSError:
            return None
        cached_key, cached_token = self._token_cache
        if cached_key == (path, mtime):
            return cached_token
        try:
            with open(path, encoding='utf8') as token_file:
                token = token_file.read().strip()
        except OSError:
            return None
        self._token_cache = ((path, mtime), token)
        return token

    def route(self, method, parts, request):
        if parts == ['torrents']:
            if method == 'GET':
                return {'torrents': [self._summary(t) for t in list(self.torrents.torrents.values())]}
            if method == 'POST':
                return self.add(_body(request))
        elif parts == ['listen-port'] and method == 'PUT':
            return self.set_listen_port(_body(request))
        elif len(parts) >= 2 and parts[0] == 'torrents':
            info_hash = _info_hash(parts[1])
            rest = parts[2:]
            if not rest:
                if method == 'GET':
                    return self._detail(self._torrent(info_hash))
                if method == 'DELETE':
                    return self.remove(info_hash, _query(request, 'remove_data') in ('1', 'true'))
            elif rest == ['pieces'] and method == 'GET':
                return self.pieces(info_hash, _query(request, 'first'), _query(request, 'last'))
            elif rest == ['wait'] and method == 'POST':
                return self.wait(info_hash, _body(request), request)
            elif rest == ['files'] and method == 'PUT':
                return self.set_files(info_hash, _body(request), request)
            elif rest == ['metainfo'] and method == 'GET':
                return self.metainfo(info_hash)
            elif len(rest) == 2 and rest[0] == 'windows':
                if method == 'PUT':
                    return self.set_window(info_hash, rest[1], _body(request))
                if method == 'DELETE':
                    return self.remove_window(info_hash, rest[1])
        raise ApiError(404, 'no such endpoint: %s /v1/%s' % (method, '/'.join(parts)))

    # --- endpoints ---

    def health(self):
        config = self.core.config
        return {
            'ok': True,
            'deluge': get_version(),
            'libtorrent': lt.__version__,
            'listen_port': self.core.get_listen_port(),
            'dht': bool(config['dht']),
            'lsd': bool(config['lsd']),
            'upnp': bool(config['upnp']),
            'natpmp': bool(config['natpmp']),
            'utpex': bool(config['utpex']),
            'pex_extension_loaded': self.pex_extension_loaded,
            'token_configured': bool(self._token()),
        }

    def add(self, body):
        wanted = body.get('wanted_files')
        if not (wanted is None or wanted == 'all' or _int_list(wanted)):
            raise ApiError(400, 'wanted_files must be null, "all" or a list of file indices')
        save_root = body.get('save_root') or os.environ.get('TEASTREAM_SAVE_ROOT') or self.config['save_root']
        if not os.path.isabs(save_root):
            raise ApiError(400, 'save_root must be an absolute path')

        torrent_b64 = body.get('torrent')
        magnet = body.get('magnet')
        if bool(torrent_b64) == bool(magnet):
            raise ApiError(400, 'provide exactly one of torrent or magnet')
        if torrent_b64:
            try:
                info = lt.torrent_info(lt.bdecode(base64.b64decode(torrent_b64, validate=True)))
            except (ValueError, TypeError, RuntimeError) as ex:
                raise ApiError(400, 'invalid torrent file: %s' % ex)
            info_hash = str(info.info_hash())
        else:
            magnet_info = get_magnet_info(magnet)
            if not magnet_info:
                raise ApiError(400, 'invalid magnet URI')
            info_hash = magnet_info['info_hash']

        existing = self.torrents.torrents.get(info_hash)
        if existing is not None:
            return {'info_hash': info_hash, 'added': False, 'save_path': existing.options['download_location']}

        save_path = os.path.join(os.path.normpath(save_root), info_hash)
        options = {
            'download_location': save_path,
            'add_paused': False,
            'auto_managed': False,
            'move_completed': False,
            'sequential_download': False,
            'prioritize_first_last_pieces': False,
            'pre_allocate_storage': False,
            'stop_at_ratio': False,
            'remove_at_ratio': False,
            'seed_mode': False,
            'super_seeding': False,
        }
        if torrent_b64:
            options['file_priorities'] = file_priorities(info.num_files(), wanted)
            torrent_id = self.core.add_torrent_file(info.name() + '.torrent', torrent_b64, options)
        else:
            self.pending_selection[info_hash] = wanted
            torrent_id = self.core.add_torrent_magnet(magnet, options)
            torrent = self.torrents.torrents.get(torrent_id)
            if torrent is not None and torrent.has_metadata:
                self._apply_pending(torrent_id)
        if not torrent_id:
            raise ApiError(500, 'Deluge did not add the torrent')
        log.info('TeaStream added torrent %s', torrent_id)
        return {'info_hash': torrent_id, 'added': True, 'save_path': save_path}

    def remove(self, info_hash, remove_data):
        torrent = self._torrent(info_hash)
        save_path = torrent.options['download_location']
        self.streams.pop(info_hash, None)
        self.pending_selection.pop(info_hash, None)
        for waiter in [w for w in self.waiters if w.info_hash == info_hash]:
            self._finish(waiter, 404, {'error': 'torrent removed'})
        if not self.core.remove_torrent(info_hash, remove_data):
            raise ApiError(500, 'Deluge could not remove the torrent')
        if remove_data and os.path.basename(os.path.normpath(save_path)) == info_hash:
            # libtorrent deletes files asynchronously and leaves our per-torrent
            # directory behind; drop it once it is empty.
            for delay in (5, 30):
                reactor.callLater(delay, _remove_empty_dirs, save_path)
        return {'removed': True}

    def pieces(self, info_hash, first, last):
        torrent = self._torrent(info_hash)
        info = self._info(torrent)
        num_pieces = info.num_pieces()
        first = _bounded_int(first, 0, 0, num_pieces - 1, 'first')
        last = _bounded_int(last, num_pieces - 1, first, num_pieces - 1, 'last')
        have = _have(torrent)
        return _pieces_response(have, first, last, all(have[first : last + 1]))

    def wait(self, info_hash, body, request):
        torrent = self._torrent(info_hash)
        num_pieces = self._info(torrent).num_pieces()
        piece = _bounded_int(body.get('piece'), None, 0, num_pieces - 1, 'piece')
        through = _bounded_int(body.get('through'), piece, piece, num_pieces - 1, 'through')
        timeout_ms = _bounded_int(body.get('timeout_ms'), 5000, 0, MAX_WAIT_MS, 'timeout_ms')
        have = _have(torrent)
        if have[piece] or timeout_ms == 0:
            return _pieces_response(have, piece, through, have[piece])
        waiter = Waiter(request, info_hash, piece, through, time.monotonic() + timeout_ms / 1000.0)
        self.waiters.append(waiter)
        request.notifyFinish().addBoth(self._forget_waiter, waiter)
        if not self.waiter_loop.running:
            self.waiter_loop.start(WAIT_POLL_SECONDS, now=False)
        return server.NOT_DONE_YET

    def set_files(self, info_hash, body, request):
        torrent = self._torrent(info_hash)
        info = self._info(torrent)
        wanted = body.get('wanted')
        if not (wanted == 'all' or isinstance(wanted, list) and (not wanted or _int_list(wanted))):
            raise ApiError(400, 'wanted must be "all" or a list of file indices')
        if isinstance(wanted, list) and any(i < 0 or i >= info.num_files() for i in wanted):
            raise ApiError(400, 'file index out of range')
        previous = list(torrent.handle.get_file_priorities())
        priorities = file_priorities(info.num_files(), wanted)
        torrent.set_file_priorities(priorities)
        self.torrents.save_state()
        if info_hash in self.streams:
            self._apply(info_hash)
        payload = {'priorities': priorities}
        if not any(old == PRIORITY_SKIP and new > PRIORITY_SKIP for old, new in zip(previous, priorities)):
            return payload
        # Pieces of a skipped file live in libtorrent's part file; the disk
        # thread moves them into the real file when the file becomes wanted.
        # Answer once file_prio_alert confirms that, so a caller that read
        # those pieces' "have" bits can read them from the file right away.
        waiter = FilePriorityWaiter(request, payload)
        self.file_priority_waiters.setdefault(info_hash, []).append(waiter)
        request.notifyFinish().addBoth(self._forget_waiter, waiter)
        reactor.callLater(FILE_PRIORITY_TIMEOUT_SECONDS, self._file_priorities_timed_out, info_hash, waiter)
        return server.NOT_DONE_YET

    def _on_file_priorities_applied(self, alert):
        try:
            info_hash = str(alert.handle.info_hash())
        except RuntimeError:
            return
        for waiter in self.file_priority_waiters.pop(info_hash, []):
            self._finish(waiter, 200, waiter.payload)

    def _file_priorities_timed_out(self, info_hash, waiter):
        waiting = self.file_priority_waiters.get(info_hash, [])
        if waiter in waiting:
            waiting.remove(waiter)
            if not waiting:
                del self.file_priority_waiters[info_hash]
        if not waiter.done:
            log.warning('TeaStream: no file_prio_alert for %s within %ds', info_hash, FILE_PRIORITY_TIMEOUT_SECONDS)
            self._finish(waiter, 200, waiter.payload)

    def set_window(self, info_hash, stream, body):
        torrent = self._torrent(info_hash)
        info = self._info(torrent)
        files = info.files()
        index = _bounded_int(body.get('file'), None, 0, files.num_files() - 1, 'file')
        size = files.file_size(index)
        if size <= 0:
            raise ApiError(400, 'file is empty')
        offset = _bounded_int(body.get('offset'), 0, 0, size - 1, 'offset')
        length = _bounded_int(body.get('length'), None, 1, 1 << 62, 'length')
        deadline_bytes = _bounded_int(body.get('deadline_bytes'), DEFAULT_DEADLINE_BYTES, 0, 1 << 62, 'deadline_bytes')
        ttl_ms = _bounded_int(body.get('ttl_ms'), DEFAULT_WINDOW_TTL_MS, 1, MAX_WINDOW_TTL_MS, 'ttl_ms')
        first, last, deadline_last = window_pieces(
            files.file_offset(index), size, info.piece_length(), info.num_pieces(),
            offset, length, deadline_bytes,
        )
        streams = self.streams.setdefault(info_hash, Streams())
        streams.windows[stream] = Window(first, last, deadline_last, time.monotonic() + ttl_ms / 1000.0)
        self._apply(info_hash)
        return {'first_piece': first, 'last_piece': last, 'deadline_last_piece': deadline_last}

    def remove_window(self, info_hash, stream):
        self._torrent(info_hash)
        streams = self.streams.get(info_hash)
        removed = streams is not None and streams.windows.pop(stream, None) is not None
        if removed:
            self._apply(info_hash)
        return {'removed': removed}

    def metainfo(self, info_hash):
        torrent = self._torrent(info_hash)
        path = os.path.join(get_config_dir(), 'state', info_hash + '.torrent')
        try:
            with open(path, 'rb') as saved:
                contents = saved.read()
        except OSError:
            info = self._info(torrent)
            trackers = [tracker['url'] for tracker in torrent.handle.trackers()]
            contents = build_metainfo(info.info_section(), trackers, lt.bencode)
        return {'torrent': base64.b64encode(contents).decode('ascii')}

    def set_listen_port(self, body):
        port = _bounded_int(body.get('port'), None, 1, 65535, 'port')
        self.core.set_config({'random_port': False, 'listen_ports': [port, port]})
        log.info('TeaStream set listen port %d', port)
        return {'port': port}

    # --- torrent state ---

    def _torrent(self, info_hash):
        torrent = self.torrents.torrents.get(info_hash)
        if torrent is None:
            raise ApiError(404, 'torrent %s not found' % info_hash)
        return torrent

    def _info(self, torrent):
        info = torrent.handle.torrent_file() if torrent.handle.is_valid() else None
        if info is None:
            raise ApiError(409, 'torrent metadata is not available yet')
        return info

    def _summary(self, torrent):
        handle = torrent.handle
        status = handle.status()
        info = handle.torrent_file() if status.has_metadata else None
        total_size = info.total_size() if info is not None else 0
        tracker_status = torrent.tracker_status or ''
        return {
            'info_hash': torrent.torrent_id,
            'name': torrent.get_name(),
            'state': torrent.state,
            'has_metadata': bool(status.has_metadata),
            'private': bool(info.priv()) if info is not None else False,
            'total_size': total_size,
            'total_done': status.total_done,
            'total_wanted': status.total_wanted,
            'total_wanted_done': status.total_wanted_done,
            'progress': min(1.0, status.total_done / total_size) if total_size else 0.0,
            'download_rate': status.download_payload_rate,
            'upload_rate': status.upload_payload_rate,
            'num_peers': status.num_peers,
            'num_seeds': status.num_seeds,
            'list_peers': status.list_peers,
            'list_seeds': status.list_seeds,
            'connect_candidates': status.connect_candidates,
            'all_time_download': status.all_time_download,
            'all_time_upload': status.all_time_upload,
            'seeding_seconds': status.seeding_time,
            'finished_seconds': status.finished_time,
            'active_seconds': status.active_time,
            'is_finished': bool(status.is_finished),
            'is_seeding': bool(status.is_seeding),
            'paused': bool(status.flags & lt.torrent_flags.paused),
            'save_path': status.save_path,
            'tracker_status': tracker_status,
            'tracker_message': tracker_status if tracker_status.startswith(('Error', 'Warning')) else '',
            'error': torrent.statusmsg if torrent.state == 'Error' else '',
            'file_progress': list(handle.file_progress(lt.torrent_handle.piece_granularity)) if info is not None else [],
        }

    def _detail(self, torrent):
        detail = self._summary(torrent)
        info = torrent.handle.torrent_file() if detail['has_metadata'] else None
        detail['trackers'] = [tracker['url'] for tracker in torrent.handle.trackers()]
        if info is None:
            detail.update(piece_length=0, num_pieces=0, files=[])
            return detail
        files = info.files()
        priorities = torrent.handle.get_file_priorities()
        progress = detail['file_progress']
        detail['piece_length'] = info.piece_length()
        detail['num_pieces'] = info.num_pieces()
        detail['files'] = [
            {
                'index': i,
                'path': files.file_path(i).replace(os.sep, '/'),
                'size': files.file_size(i),
                'offset': files.file_offset(i),
                'pad': bool(files.file_flags(i) & lt.file_storage.flag_pad_file),
                'priority': priorities[i] if i < len(priorities) else 0,
                'done': progress[i] if i < len(progress) else 0,
            }
            for i in range(files.num_files())
        ]
        return detail

    def _on_metadata_received(self, alert):
        try:
            info_hash = str(alert.handle.info_hash())
        except RuntimeError:
            return
        if info_hash in self.pending_selection:
            # Run after TorrentManager's own handler has marked the metadata.
            reactor.callLater(0, self._apply_pending, info_hash)

    def _apply_pending(self, info_hash, attempts=20):
        if info_hash not in self.pending_selection:
            return
        torrent = self.torrents.torrents.get(info_hash)
        if torrent is None:
            self.pending_selection.pop(info_hash, None)
            return
        if not torrent.has_metadata:
            if attempts > 0:
                reactor.callLater(0.1, self._apply_pending, info_hash, attempts - 1)
            return
        wanted = self.pending_selection.pop(info_hash)
        torrent.set_file_priorities(file_priorities(torrent.torrent_info.num_files(), wanted))
        if info_hash in self.streams:
            self._apply(info_hash)

    def _apply(self, info_hash):
        """Push the merged windows of a torrent to libtorrent."""
        streams = self.streams.get(info_hash)
        torrent = self.torrents.torrents.get(info_hash)
        if streams is None:
            return
        if torrent is None or not torrent.handle.is_valid():
            del self.streams[info_hash]
            return
        handle = torrent.handle
        info = handle.torrent_file()
        if info is None:
            return
        have = _have(torrent)
        windows = [(w.first, w.last, w.deadline_last) for w in streams.windows.values()]
        plan = schedule(windows, have, info.piece_length())
        deadlines, zones, boosted = plan
        for piece in streams.deadlines - deadlines.keys():
            handle.reset_piece_deadline(piece)
        for piece, due in deadlines.items():
            handle.set_piece_deadline(piece, due)
        streams.deadlines = set(deadlines)

        files = info.files()
        base = base_piece_priorities(
            [
                (files.file_offset(i), files.file_size(i), bool(files.file_flags(i) & lt.file_storage.flag_pad_file))
                for i in range(files.num_files())
            ],
            handle.get_file_priorities(),
            info.num_pieces(),
            info.piece_length(),
        )
        downloaded = _downloaded(handle) if zones else {}
        priorities = stream_priorities(base, windows, have, plan, downloaded, info.piece_length())
        if boosted or streams.boosted:
            if list(handle.get_piece_priorities()) != priorities:
                handle.prioritize_pieces(priorities)
        streams.boosted = bool(boosted)
        if not streams.windows:
            del self.streams[info_hash]

    # --- long-poll waiters ---

    def _poll_waiters(self):
        now = time.monotonic()
        have_by_hash = {}
        for waiter in list(self.waiters):
            if waiter.done:
                continue
            if waiter.info_hash not in have_by_hash:
                torrent = self.torrents.torrents.get(waiter.info_hash)
                have_by_hash[waiter.info_hash] = _have(torrent) if torrent is not None else None
            have = have_by_hash[waiter.info_hash]
            if have is None:
                self._finish(waiter, 404, {'error': 'torrent removed'})
            elif have[waiter.piece] or waiter.expires <= now:
                self._finish(
                    waiter, 200, _pieces_response(have, waiter.piece, waiter.through, have[waiter.piece])
                )
        self.waiters = [w for w in self.waiters if not w.done]
        if not self.waiters and self.waiter_loop.running:
            self.waiter_loop.stop()

    def _finish(self, waiter, code, payload):
        if waiter.done:
            return
        waiter.done = True
        request = waiter.request
        request.setResponseCode(code)
        request.setHeader('content-type', 'application/json')
        request.write(json.dumps(payload).encode())
        request.finish()

    def _forget_waiter(self, result, waiter):
        # Fires when the response finished or the client went away.
        waiter.done = True
        return None


class _Api(resource.Resource):
    isLeaf = True

    def __init__(self, plugin):
        super().__init__()
        self.plugin = plugin

    def render(self, request):
        try:
            method = request.method.decode('ascii')
            parts = [part for part in request.path.decode('utf8').split('/') if part]
            if parts[:1] != ['v1']:
                raise ApiError(404, 'not found')
            parts = parts[1:]
            if parts == ['health'] and method == 'GET':
                return _respond(request, 200, self.plugin.health())
            self.plugin.authorize(request)
            result = self.plugin.route(method, parts, request)
            if result is server.NOT_DONE_YET:
                return result
            return _respond(request, 200, result)
        except ApiError as ex:
            return _respond(request, ex.code, {'error': ex.message})
        except Exception as ex:  # noqa: BLE001 - the API must answer, not crash the daemon
            log.exception('TeaStream request failed')
            return _respond(request, 500, {'error': str(ex)})


class _QuietSite(server.Site):
    noisy = False

    def log(self, request):
        pass


def _respond(request, code, payload):
    request.setResponseCode(code)
    request.setHeader('content-type', 'application/json')
    return json.dumps(payload).encode()


def _body(request):
    raw = request.content.read() if request.content is not None else b''
    if not raw:
        return {}
    try:
        body = json.loads(raw)
    except ValueError as ex:
        raise ApiError(400, 'invalid JSON body: %s' % ex)
    if not isinstance(body, dict):
        raise ApiError(400, 'JSON body must be an object')
    return body


def _query(request, name):
    values = request.args.get(name.encode())
    return values[0].decode('utf8') if values else None


def _info_hash(value):
    value = value.lower()
    if len(value) != 40 or any(c not in '0123456789abcdef' for c in value):
        raise ApiError(400, 'invalid info hash')
    return value


def _int_list(value):
    return isinstance(value, list) and all(isinstance(i, int) and not isinstance(i, bool) for i in value)


def _bounded_int(value, default, low, high, name):
    if value is None:
        if default is None:
            raise ApiError(400, '%s is required' % name)
        return default
    try:
        number = int(value)
    except (TypeError, ValueError):
        raise ApiError(400, '%s must be an integer' % name)
    if number < low or number > high:
        raise ApiError(400, '%s must be between %d and %d' % (name, low, high))
    return number


def _have(torrent):
    return torrent.handle.status(lt.torrent_handle.query_pieces).pieces


# libtorrent block states: none, requested, writing, finished.
_BLOCK_RECEIVED = (2, 3)


def _downloaded(handle):
    """Bytes already received of each partially downloaded piece."""
    return {
        partial['piece_index']: sum(block['block_size'] for block in partial['blocks'] if block['state'] in _BLOCK_RECEIVED)
        for partial in handle.get_download_queue()
    }


def _pieces_response(have, first, last, complete):
    return {
        'first': first,
        'last': last,
        'bitfield': base64.b64encode(pack_bitfield(have, first, last)).decode('ascii'),
        'complete': bool(complete),
    }


def _remove_empty_dirs(path):
    for root, _dirs, _files in os.walk(path, topdown=False):
        try:
            os.rmdir(root)
        except OSError:
            pass
