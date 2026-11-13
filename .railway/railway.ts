import { defineRailway, github, project, service } from "railway/iac";
// This repository manages only its own resources in the environment. Other
// repositories export their own partial name.
// See https://docs.railway.com/infrastructure-as-code#multi-repo-projects
export const partial = "backend";
export default defineRailway(() => {
  const backend = service("backend", {
    source: github("TrackLines/backend"),
    build: { buildEnvironment: "V3", builder: "DOCKERFILE", dockerfilePath: "Dockerfile" },
    healthcheck: "/health",
    healthcheckTimeout: 300,
    // zero-downtime deploys (TL-896): the old deploy keeps serving for 30s, then gets 20s between
    // SIGTERM and SIGKILL to finish serveOn's 15s drain (Railway's defaults are 0 and 0)
    deploy: { overlapSeconds: 30, drainingSeconds: 20 },
    replicas: { "europe-west4-drams3a": 1 },
    domains: ["api.tracklin.es"],
  });

  return project("Tracklines", {
    variables: { managed: false },
    resources: [backend],
  });
});
