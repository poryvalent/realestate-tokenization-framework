#!/usr/bin/env bash
# Shared environment for running Foundry from WSL against the Windows checkout.
#
# Two problems this works around.
#
# Windows HOME leaks into the WSL environment as "C:Usersaquap", so bash looks for
# its profile at a path that does not exist and never adds Foundry to PATH. HOME is
# therefore reset explicitly rather than relying on a login shell.
#
# PowerShell mangles shell metacharacters when passing a command string to wsl, so
# every non-trivial invocation lives in a script file and is sourced or executed
# rather than inlined.

export HOME=/home/aquap
export PATH="$HOME/.foundry/bin:$PATH"

# The checkout lives on the Windows filesystem. Paths contain a space, so every
# expansion of this must be quoted.
export ACRESYNC_ROOT="/mnt/c/Users/aquap/Desktop/New folder/AcreSync"
export CONTRACTS_DIR="$ACRESYNC_ROOT/contracts"
