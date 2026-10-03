// REST API client for tests (Node 18+ fetch).
export class ApiError extends Error {
  constructor(status, message) {
    super(`${status}: ${message}`);
    this.status = status;
  }
}

export class Api {
  constructor(baseUrl) {
    this.baseUrl = baseUrl;
  }

  async request(method, path, body) {
    const opts = { method };
    if (body !== undefined) {
      opts.body = typeof body === 'string' ? body : JSON.stringify(body);
      opts.headers = { 'Content-Type': 'application/json' };
    }
    const res = await fetch(this.baseUrl + path, opts);
    let json = null;
    try {
      json = await res.json();
    } catch {}
    if (!res.ok) {
      throw new ApiError(res.status, json?.error || `(no error body)`);
    }
    return json;
  }

  // Servers
  listServers() {
    return this.request('GET', '/api/servers');
  }
  addServer(fields) {
    return this.request('POST', '/api/servers', fields);
  }
  deleteServer(id) {
    return this.request('DELETE', `/api/servers/${id}`);
  }

  // VMs
  listVMs(serverId) {
    return this.request('GET', `/api/servers/${serverId}/vms`);
  }
  async ping(serverId) {
    return this.request('GET', `/api/servers/${serverId}/ping`);
  }
  async action(serverId, name, action) {
    await this.request(
      'POST',
      `/api/servers/${serverId}/vms/${encodeURIComponent(name)}/${action}`,
    );
  }
}
