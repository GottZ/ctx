"""Live measurements of the release wave W02-15 against a RUNNING ctx
(design/02 §7 W02-15; masterplan §8 open measurements; checklist.json).

Run once by the main context in RC-D S4, never in CI. Engine-side
measurements (SGLang num_running_reqs after a client abort, n = 8,
array-embed slots) are the bench tool of A03-W11 outside the repository
(.project/plan-openai-api-2026-11-03/bench/); this script measures what a
client of ctx sees. Standard library only; keys from environment variables.

Subcommands (contract; RC-D S1 ships them as stubs that fail closed). Each
prints ONE JSON object on stdout and exits 0 when the checklist threshold
holds, 1 when it does not or the measurement failed, 2 on usage:

``--list``
    print the subcommand ids one per line.
``proxy --base-url URL``
    a request through the reverse proxy with an invalid model key:
    ``{"verdict": "trusted_proxy_unset"}`` (503 proxy_misconfigured — exit 1)
    or ``{"verdict": "trusted_proxy_set"}`` (the door's 401 — exit 0);
    anything else ``{"verdict": "unexpected", "status": …}`` (exit 1).
``prefill --base-url URL --key-env NAME --model M --tokens N``
    ``{"prefill_s": …, "first_byte_s": …, "cut_off": bool}`` — exit 1 when
    the stream was cut before its first token.
``heap --base-url URL --key-env NAME --model M --streams 64 --consumers-keys-env NAME --mem-file PATH --budget-bytes B``
    flood while sampling the container's memory (``--mem-file``: a cgroup
    ``memory.current`` file or a file a sampler rewrites):
    ``{"base_bytes": …, "peak_bytes": …, "budget_bytes": …, "ok": bool}``,
    ok = peak − base < budget.
``retry-after --base-url URL --key-env NAME --model M --requests N``
    ``{"requests": …, "share_429": …, "retry_after": [...], "ok": bool}``,
    ok = every 429 carries Retry-After in [1, 30] and x-should-retry.
"""

import argparse
import json
import sys

EXIT_PASS = 0
EXIT_FAIL = 1
EXIT_USAGE = 2

# The subcommands this script implements (printed by --list). RC-D S1: none.
SUBCOMMANDS: list[str] = []


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="measure.py", description=__doc__.splitlines()[0])
    p.add_argument("--list", action="store_true", help="print the implemented subcommand ids")
    sub = p.add_subparsers(dest="cmd")
    px = sub.add_parser("proxy")
    px.add_argument("--base-url", required=True)
    pf = sub.add_parser("prefill")
    pf.add_argument("--base-url", required=True)
    pf.add_argument("--key-env", required=True)
    pf.add_argument("--model", required=True)
    pf.add_argument("--tokens", type=int, default=200_000)
    hp = sub.add_parser("heap")
    hp.add_argument("--base-url", required=True)
    hp.add_argument("--key-env", required=True)
    hp.add_argument("--model", required=True)
    hp.add_argument("--streams", type=int, default=64)
    hp.add_argument("--consumers-keys-env", required=True)
    hp.add_argument("--mem-file", required=True)
    hp.add_argument("--budget-bytes", type=int, required=True)
    ra = sub.add_parser("retry-after")
    ra.add_argument("--base-url", required=True)
    ra.add_argument("--key-env", required=True)
    ra.add_argument("--model", required=True)
    ra.add_argument("--requests", type=int, default=200)
    return p


def main(argv: list[str]) -> int:
    args = _parser().parse_args(argv)
    if args.list:
        for s in SUBCOMMANDS:
            print(s)
        return EXIT_PASS
    if args.cmd is None:
        _parser().print_usage(sys.stderr)
        return EXIT_USAGE
    # RC-D S1 stub: fail closed.
    print(json.dumps({"measure": args.cmd, "error": "not implemented (RC-D S1 stub)"}))
    return EXIT_FAIL


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
