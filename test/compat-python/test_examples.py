"""Every ```python block in docs/examples/python.md, run as written.

A snippet that is documented and never run is a claim nothing checks, so the
page is the test's input: a new block is run, a changed block is run, and a
block that fails fails the suite.
"""

import pathlib
import re

import pytest

import conftest

PAGE = pathlib.Path(__file__).resolve().parents[2] / "docs" / "examples" / "python.md"
BLOCKS = re.findall(r"```python\n(.*?)```", PAGE.read_text(), flags=re.S)


def test_the_page_has_examples():
    assert len(BLOCKS) >= 4, f"expected the four service examples in {PAGE}"


@pytest.mark.parametrize("index", range(len(BLOCKS)))
def test_example(index):
    code = BLOCKS[index]
    # The gRPC examples open their channels in the snippet itself, so the
    # loopback guard checks each target the way the suite's own tests do.
    for target in re.findall(r'os\.environ\["(CLOUDBURROW_[A-Z]+_ENDPOINT)"\]', code):
        conftest.channel_target(__import__("os").environ[target])
    exec(compile(code, f"{PAGE.name} block {index + 1}", "exec"), {"__name__": f"example_{index}"})
