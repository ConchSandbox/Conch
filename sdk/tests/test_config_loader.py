from conch.config_loader import clear_config_cache, load_config


def test_load_config_uses_environment_path_and_cache(tmp_path, monkeypatch):
    config_path = tmp_path / "sdk-config.yaml"
    config_path.write_text("sandbox:\n  api_url: http://first.example\n", encoding="utf-8")
    monkeypatch.setenv("CONCH_SDK_CONFIG", str(config_path))
    clear_config_cache()

    try:
        first = load_config()
        config_path.write_text("sandbox:\n  api_url: http://second.example\n", encoding="utf-8")
        second = load_config()
    finally:
        clear_config_cache()

    assert first["sandbox"]["api_url"] == "http://first.example"
    assert second["sandbox"]["api_url"] == "http://first.example"
