"""Texts of the R6 fixes as the user sees them: a size refusal that names its setting is shown as it is (no proxy
advice), and an answer refused while reading is not called a signature problem."""

from __future__ import annotations

from ctx_handover.engine import surfaces
from ctx_handover.engine.errors import BadResponseSignature, PayloadTooLarge


def test_r5_size_refusal_with_a_setting_keeps_its_own_advice():
    msg = (
        "This handover is 12.0 GiB, but server accepts at most 10.0 GiB of files (its setting max_total_gb) and 512 "
        "MiB of conversation per handover. Nothing was sent. Raise max_total_gb on server, or hand over less "
        "(--exclude, --no-workspace)."
    )
    text = surfaces.render_error(PayloadTooLarge(msg, setting="max_total_gb", bytes=1, max_bytes=0), "server")
    assert text == msg
    assert "proxy" not in text


def test_r5_size_refusal_without_a_setting_still_names_the_proxy():
    text = surfaces.render_error(PayloadTooLarge("request body too large"), "server")
    assert "proxy" in text


def test_r5_answer_refused_while_reading_is_not_a_signature_problem():
    err = BadResponseSignature("the answer from server is larger than this machine accepts", max_bytes=64)
    text = surfaces.render_error(err, "server")
    assert text.startswith("The answer from server is larger than this machine accepts, so this machine did not read")
    assert "not signed" not in text


def test_r5_a_wrong_signature_still_says_so():
    text = surfaces.render_error(BadResponseSignature("response signature invalid"), "server")
    assert "not signed with the key" in text
