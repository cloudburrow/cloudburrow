"""Run only by test_guard.py, as its own pytest session: a request to a
non-loopback address, caught, must still fail the session."""

import socket

from conftest import GuardError


def test_a_caught_leak():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.connect(("192.0.2.1", 443))  # TEST-NET-1: never routed
    except GuardError:
        pass
    finally:
        s.close()
