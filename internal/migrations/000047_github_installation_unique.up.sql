-- A GitHub App installation belongs to one org: connecting it elsewhere is refused (TL-158).
CREATE UNIQUE INDEX org_integrations_github_installation_key ON org_integrations (external_id) WHERE provider = 'github';
