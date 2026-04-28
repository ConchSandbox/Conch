def test_execute_python_content(sandbox):
    """Execute Python script via content parameter."""
    result = sandbox.execute(cmd="python3", content='print("hello")')
    assert result.exit_code == 0
    assert "hello" in result.stdout


def test_execute_args(sandbox):
    """Execute command with args."""
    result = sandbox.execute(cmd="ls", args=["-l", "/root"])
    assert result.exit_code == 0


def test_execute_cwd(sandbox):
    """Execute command with specified working directory."""
    result = sandbox.execute(cmd="sh", args=["-c", "pwd"], cwd="/tmp")
    assert result.exit_code == 0
    assert "/tmp" in result.stdout.strip()


def test_execute_env_with_shell(sandbox):
    """Execute command with environment variables via sh -c."""
    result = sandbox.execute(cmd="sh", args=["-c", "echo $MY_VAR"],
                             env={"MY_VAR": "conch_test"})
    assert result.exit_code == 0
    assert "conch_test" in result.stdout


def test_execute_exit_code(sandbox):
    """Non-zero exit code is captured."""
    result = sandbox.execute(cmd="ls", args=["/nonexistent_path"])
    assert result.exit_code != 0
    assert result.stderr != ""


def test_execute_no_shell_expansion(sandbox):
    """Verify $VAR is NOT expanded without shell (FAQ case)."""
    result = sandbox.execute(cmd="echo", args=["$HOME"])
    assert result.exit_code == 0
    assert "$HOME" in result.stdout
