"""Serve one kagent Declarative agent from a mounted config dir without the kagent controller.

Runs inside the production kagent-app image under its own venv python, so model
client, ADK, MCP toolset and A2A server are byte for byte the prod ones. The only
difference from `kagent-adk static` is `build(local=True)`: in-memory sessions and
task store instead of the controller's, which is what a laptop or CI job has.

Reads `<AGENTRUN_CONFIG>/config.json` and `agent-card.json` in the same shape the
controller renders into the agent Secret. KAGENT_URL/NAME/NAMESPACE must be set
(KAgentConfig insists); the URL is never called in local mode.
"""

import json
import os

import uvicorn
from a2a.types import AgentCard
from kagent.adk import AgentConfig, KAgentApp
from kagent.core import KAgentConfig, configure_logging


def main() -> None:
    configure_logging()
    app_cfg = KAgentConfig()
    config_dir = os.environ.get("AGENTRUN_CONFIG", "/config")
    with open(os.path.join(config_dir, "config.json"), encoding="utf-8") as handle:
        agent_config = AgentConfig.model_validate(json.load(handle))
    with open(os.path.join(config_dir, "agent-card.json"), encoding="utf-8") as handle:
        agent_card = AgentCard.model_validate(json.load(handle))

    def root_agent_factory():
        return agent_config.to_agent(app_cfg.name, None, False)

    kagent_app = KAgentApp(
        root_agent_factory,
        agent_card,
        app_cfg.url,
        app_cfg.app_name,
        stream=bool(agent_config.stream),
        agent_config=agent_config,
    )
    uvicorn.run(
        kagent_app.build(local=True),
        host="0.0.0.0",
        port=int(os.environ.get("PORT", "8080")),
        log_level=os.environ.get("LOG_LEVEL", "info").lower(),
    )


if __name__ == "__main__":
    main()
