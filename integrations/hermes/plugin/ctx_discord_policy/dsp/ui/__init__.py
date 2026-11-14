"""The ``/policy`` interface in Discord (DESIGN §8).

Importing this package never imports discord.py: Hermes installs without the Discord extra must still load
the plugin. Everything that needs discord.py receives the module from :func:`commands.install`.
"""
