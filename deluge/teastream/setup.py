#
# TeaStream: streaming control API plugin for Deluge 2.
#
# Deluge plugins import Deluge and are therefore distributed under the GNU
# General Public License 3.0 or later, like Deluge itself.
#

from setuptools import find_packages, setup

__plugin_name__ = 'TeaStream'
__author__ = 'pgeske'
__version__ = '1.0'
__url__ = 'https://github.com/pgeske/filmstream'
__license__ = 'GPLv3'
__description__ = 'Local HTTP API that lets Filmstream stream torrents through Deluge.'
__long_description__ = __description__

setup(
    name=__plugin_name__,
    version=__version__,
    description=__description__,
    author=__author__,
    url=__url__,
    license=__license__,
    long_description=__long_description__,
    packages=find_packages(),
    entry_points="""
    [deluge.plugin.core]
    %s = deluge_teastream:CorePlugin
    """
    % __plugin_name__,
)
