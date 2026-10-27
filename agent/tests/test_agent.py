"""Tests for agent instruction and configuration."""

from ursa_agent.agent import INSTRUCTION


def test_instruction_allocation_and_neutral_cost_wording():
    # Phase F3: researcher views lead with work; allocations are warn-only; neutral node sharing wording
    assert "my_allocation" in INSTRUCTION
    assert "bill whole nodes" not in INSTRUCTION
    instruction_normalized = " ".join(INSTRUCTION.split())
    assert (
        "Allocations are covered by Research Computing / UCR ITS and are warn-only" in instruction_normalized
    )
    assert "Never show the cluster's own $/hour to someone without staff access" in instruction_normalized
