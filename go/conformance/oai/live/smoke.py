"""Live smoke of the OpenAI-compatible API against a RUNNING ctx (waves W02-15,
W06-19 stage 2, W06-20; design/02 §7 W02-15, design/06 §7 W06-19/W06-20).

The in-process harness (internal/oai/pyconformance_test.go) checks the
handlers against fakes; this script checks a deployed stack the way an
external program meets it. test.sh T25 runs ``t25``; the release checklist
(checklist.json) names the other subcommands. Standard library plus the
official OpenAI Python SDK (``uv run --no-project --with openai==3.42.1``);
keys come from environment variables only, never from the command line.

Subcommands (contract; RC-D S1 ships them as stubs that fail closed):

``--list``
    print the subcommand ids one per line (the checklist's ``tool`` column).
``preflight --base-url URL --admin-key-env NAME``
    read /health and /api/status: release deployed, no suspected foreign
    load, the partition (internal_reserved_slots) on the GPU targets.
``flip-probe --base-url URL --expect off|on``
    anonymous ``POST /v1/chat/completions``: ``off`` wants 404 not_found in
    the OpenAI envelope with x-should-retry false (a 401 means the door runs
    before the gate); ``on`` wants 401 invalid_api_key with
    ``WWW-Authenticate: Bearer realm="ctx"``; a 503 proxy_misconfigured names
    CTX_TRUSTED_PROXY.
``run --base-url URL --key-env NAME [--chat-model M --embed-model M --rerank-model M]``
    the probes with a model key: models, chat, chat_stream, embeddings,
    rerank, unauth, envelope.
``t25 --base-url URL --admin-key-env NAME``
    anonymous probe first — 404 not_found ⇒ SKIP (gateway.enabled is false;
    no consumer is created); otherwise create a test consumer
    (model-consumer-create, all_models), ``run`` with its key, delete it
    (model-consumer-delete) in every case.

Exit codes: 0 PASS · 77 SKIP (reason on the last line, ``SKIP: …``) ·
1 FAIL (``FAIL: <probe>: <reason>``) · 2 usage. The last stdout line is the
verdict; ``--json`` adds one JSON object per probe before it.
"""

import argparse
import sys

EXIT_PASS = 0
EXIT_FAIL = 1
EXIT_USAGE = 2
EXIT_SKIP = 77

# The subcommands this script implements (printed by --list). RC-D S1: none
# yet — the stub lists nothing, so the checklist test sees every tool open.
SUBCOMMANDS: list[str] = []


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="smoke.py", description=__doc__.splitlines()[0])
    p.add_argument("--list", action="store_true", help="print the implemented subcommand ids")
    p.add_argument("--json", action="store_true", help="one JSON object per probe before the verdict")
    sub = p.add_subparsers(dest="cmd")
    pre = sub.add_parser("preflight")
    pre.add_argument("--base-url", required=True)
    pre.add_argument("--admin-key-env", required=True)
    fp = sub.add_parser("flip-probe")
    fp.add_argument("--base-url", required=True)
    fp.add_argument("--expect", choices=("off", "on"), required=True)
    run = sub.add_parser("run")
    run.add_argument("--base-url", required=True)
    run.add_argument("--key-env", required=True)
    run.add_argument("--chat-model", default="chat")
    run.add_argument("--embed-model", default="embed")
    run.add_argument("--rerank-model", default="rerank")
    t25 = sub.add_parser("t25")
    t25.add_argument("--base-url", required=True)
    t25.add_argument("--admin-key-env", required=True)
    t25.add_argument("--chat-model", default="chat")
    t25.add_argument("--embed-model", default="embed")
    t25.add_argument("--rerank-model", default="rerank")
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
    # RC-D S1 stub: fail closed, never a PASS and never a SKIP.
    print(f"FAIL: {args.cmd}: not implemented (RC-D S1 stub)")
    return EXIT_FAIL


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
