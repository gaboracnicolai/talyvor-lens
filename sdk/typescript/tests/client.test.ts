import { LensClient, injectLensHeaders } from "../src";

describe("LensClient", () => {
  it("throws when lensUrl is missing", () => {
    expect(() => new LensClient({ lensUrl: "", apiKey: "tlv_x" })).toThrow();
  });

  it("throws when apiKey is missing", () => {
    expect(
      () => new LensClient({ lensUrl: "http://lens:8080", apiKey: "" }),
    ).toThrow();
  });

  it("strips trailing slash from lensUrl", () => {
    const c = new LensClient({
      lensUrl: "http://lens:8080/",
      apiKey: "tlv_x",
    });
    expect(c.lensUrl).toBe("http://lens:8080");
  });

  it("sets Authorization header", () => {
    const c = new LensClient({
      lensUrl: "http://lens:8080",
      apiKey: "tlv_secret",
    });
    expect(c.getHeaders()["Authorization"]).toBe("Bearer tlv_secret");
  });

  it("sets X-Talyvor-Workspace header", () => {
    const c = new LensClient({
      lensUrl: "http://lens:8080",
      apiKey: "tlv_x",
      workspaceId: "finance",
    });
    expect(c.getHeaders()["X-Talyvor-Workspace"]).toBe("finance");
  });

  it("defaults workspace to 'default' when unset", () => {
    const c = new LensClient({ lensUrl: "http://lens:8080", apiKey: "tlv_x" });
    expect(c.getHeaders()["X-Talyvor-Workspace"]).toBe("default");
  });

  it("omits optional headers when not set", () => {
    const c = new LensClient({ lensUrl: "http://lens:8080", apiKey: "tlv_x" });
    const headers = c.getHeaders();
    expect(headers["X-Talyvor-Team"]).toBeUndefined();
    expect(headers["X-Talyvor-Feature"]).toBeUndefined();
    expect(headers["X-Talyvor-Session"]).toBeUndefined();
    expect(headers["X-Talyvor-Agent"]).toBeUndefined();
    expect(headers["X-Talyvor-Branch"]).toBeUndefined();
  });

  it("includes optional attribution headers when provided", () => {
    const c = new LensClient({
      lensUrl: "http://lens:8080",
      apiKey: "tlv_x",
      workspaceId: "ws",
      team: "core",
      feature: "search",
      sessionId: "sess-42",
      agentName: "ranger",
      branch: "main",
    });
    const headers = c.getHeaders();
    expect(headers["X-Talyvor-Team"]).toBe("core");
    expect(headers["X-Talyvor-Feature"]).toBe("search");
    expect(headers["X-Talyvor-Session"]).toBe("sess-42");
    expect(headers["X-Talyvor-Agent"]).toBe("ranger");
    expect(headers["X-Talyvor-Branch"]).toBe("main");
  });

  it("withSession returns a new client with session header", () => {
    const parent = new LensClient({
      lensUrl: "http://lens:8080",
      apiKey: "tlv_x",
      workspaceId: "ws",
      team: "core",
    });
    const child = parent.withSession("sess-99", "planner");

    expect(child).not.toBe(parent);
    expect(child.getHeaders()["X-Talyvor-Session"]).toBe("sess-99");
    expect(child.getHeaders()["X-Talyvor-Agent"]).toBe("planner");
    // Workspace + team carry across.
    expect(child.getHeaders()["X-Talyvor-Workspace"]).toBe("ws");
    expect(child.getHeaders()["X-Talyvor-Team"]).toBe("core");
    // Parent unchanged.
    expect(parent.getHeaders()["X-Talyvor-Session"]).toBeUndefined();
  });

  it("withBranch returns a new client with branch and PR headers", () => {
    const parent = new LensClient({
      lensUrl: "http://lens:8080",
      apiKey: "tlv_x",
    });
    const child = parent.withBranch("feat/login", "42");

    expect(child.getHeaders()["X-Talyvor-Branch"]).toBe("feat/login");
    expect(child.getHeaders()["X-Talyvor-PR"]).toBe("42");
    expect(parent.getHeaders()["X-Talyvor-Branch"]).toBeUndefined();
  });

  it("getHeaders returns a copy that cannot mutate internal state", () => {
    const c = new LensClient({ lensUrl: "http://lens:8080", apiKey: "tlv_x" });
    const headers = c.getHeaders();
    headers["X-Tamper"] = "yes";
    expect(c.getHeaders()["X-Tamper"]).toBeUndefined();
  });
});

describe("injectLensHeaders", () => {
  it("merges with existing headers without mutating the input", () => {
    const existing = { "User-Agent": "MyApp/1.0", Accept: "application/json" };
    const headers = injectLensHeaders(existing, { apiKey: "tlv_x" });

    expect(headers["User-Agent"]).toBe("MyApp/1.0");
    expect(headers["Accept"]).toBe("application/json");
    expect(headers["Authorization"]).toBe("Bearer tlv_x");
    // Caller's dict must NOT be mutated.
    expect((existing as Record<string, string>)["Authorization"]).toBeUndefined();
  });

  it("omits undefined optional fields", () => {
    const headers = injectLensHeaders(
      {},
      { apiKey: "tlv_x", workspaceId: "ws" },
    );
    expect(headers["X-Talyvor-Team"]).toBeUndefined();
    expect(headers["X-Talyvor-Feature"]).toBeUndefined();
    expect(headers["X-Talyvor-Session"]).toBeUndefined();
  });

  it("includes all provided fields", () => {
    const headers = injectLensHeaders(
      {},
      {
        apiKey: "tlv_x",
        workspaceId: "ws",
        team: "core",
        feature: "search",
        sessionId: "sess-1",
        agentName: "ranger",
        branch: "feat/login",
        prNumber: "42",
        commit: "abc123",
        repository: "org/repo",
      },
    );
    expect(headers["X-Talyvor-Team"]).toBe("core");
    expect(headers["X-Talyvor-Feature"]).toBe("search");
    expect(headers["X-Talyvor-Session"]).toBe("sess-1");
    expect(headers["X-Talyvor-Agent"]).toBe("ranger");
    expect(headers["X-Talyvor-Branch"]).toBe("feat/login");
    expect(headers["X-Talyvor-PR"]).toBe("42");
    expect(headers["X-Talyvor-Commit"]).toBe("abc123");
    expect(headers["X-Talyvor-Repository"]).toBe("org/repo");
  });

  it("defaults workspace to 'default' when empty", () => {
    const headers = injectLensHeaders({}, { apiKey: "tlv_x" });
    expect(headers["X-Talyvor-Workspace"]).toBe("default");
  });
});
