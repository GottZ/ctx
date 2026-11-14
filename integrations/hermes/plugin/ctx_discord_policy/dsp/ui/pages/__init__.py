"""Settings panels of ``/policy``, one module each. Importing this package registers them.

Add a panel: create ``pages/<name>.py`` following :mod:`.progress` and import it below.
"""

from . import channels, guidance, identity, lanes, managers, operator_lane, progress, worklog  # noqa: F401
from . import model, profile  # noqa: F401  (S12/S13: Hermes profile and models of the server)
