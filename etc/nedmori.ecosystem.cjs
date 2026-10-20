module.exports = {
  apps: [{
    name: "nedmori-api",
    cwd: "/opt/Nedmori/current",
    script: "/opt/Nedmori/current/.venv/bin/python",
    args: "-m nedmori",
    interpreter: "none",
    exec_mode: "fork",
    instances: 1,
    autorestart: true,
    min_uptime: "10s",
    max_restarts: 5,
    restart_delay: 3000,
    kill_timeout: 30000,
    env: { NEDMORI_API_PORT: "8791", NEDMORI_INSTANCE_ID: "nedmori-production" },
  }],
};
