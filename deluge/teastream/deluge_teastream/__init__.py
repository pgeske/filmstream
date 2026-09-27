#
# TeaStream: streaming control API plugin for Deluge 2.
# SPDX-License-Identifier: GPL-3.0-or-later
#

from deluge.plugins.init import PluginInitBase


class CorePlugin(PluginInitBase):
    def __init__(self, plugin_name):
        from .core import Core as _pluginCls

        self._plugin_cls = _pluginCls
        super().__init__(plugin_name)
