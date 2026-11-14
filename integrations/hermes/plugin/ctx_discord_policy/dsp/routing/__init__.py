"""Hermes profile per Discord server (PROFILE-PER-GUILD Teil A, layer S12).

The plugin manages its own, name-marked entries in Hermes' ``profile_routes``: persisted through Hermes'
configuration API in the default root's config.yaml and, in the same step, applied to the running
gateway by replacing ``runner.config.profile_routes`` with a list from Hermes' own parser.

* :mod:`.table`       — plugin entries in a route table (pure functions, no Hermes);
* :mod:`.eligibility` — which profiles the operator may choose, and why not the others;
* :mod:`.apply`       — set / take over / re-apply a server's profile (config + live + store);
* :mod:`.check`       — A4-P, the per-message check that public turns run where the operator chose.
"""
