import { describe, expect, it } from "vitest";

import { buildSettings, credentialKind, needsCredentials, typeLabel } from "./destinations";

describe("buildSettings", () => {
  it("keeps only the fields a local destination uses", () => {
    expect(
      buildSettings("local", { path: "/mnt/backups", bucket: "ignored", endpoint: "ignored" }),
    ).toEqual({ path: "/mnt/backups" });
  });

  it("drops empty fields", () => {
    expect(buildSettings("local", { path: "" })).toEqual({});
    expect(buildSettings("s3", { endpoint: "s3.amazonaws.com", bucket: "", region: "  " })).toEqual({
      endpoint: "s3.amazonaws.com",
    });
  });

  it("trims string values", () => {
    expect(buildSettings("b2", { endpoint: " s3.us-west-004.backblazeb2.com ", bucket: " my-bucket " })).toEqual({
      endpoint: "s3.us-west-004.backblazeb2.com",
      bucket: "my-bucket",
    });
  });

  it("builds S3 settings with an optional region and path", () => {
    expect(
      buildSettings("s3", { endpoint: "s3.amazonaws.com", bucket: "my-bucket", region: "us-east-1", path: "ballast" }),
    ).toEqual({ endpoint: "s3.amazonaws.com", bucket: "my-bucket", region: "us-east-1", path: "ballast" });
  });

  it("builds R2 settings from the account id and bucket, without an endpoint field", () => {
    expect(buildSettings("r2", { account_id: "abc123", bucket: "my-bucket", endpoint: "ignored" })).toEqual({
      account_id: "abc123",
      bucket: "my-bucket",
    });
  });

  it("parses the SFTP port from a string and drops it when unset", () => {
    expect(buildSettings("sftp", { host: "nas.lan", user: "ballast", path: "/srv/backups", port: "2222" })).toEqual({
      host: "nas.lan",
      user: "ballast",
      path: "/srv/backups",
      port: 2222,
    });
    expect(buildSettings("sftp", { host: "nas.lan", user: "ballast", path: "/srv/backups", port: "" })).toEqual({
      host: "nas.lan",
      user: "ballast",
      path: "/srv/backups",
    });
  });

  it("keeps only the url for a custom destination", () => {
    expect(buildSettings("custom", { url: "rest:https://backup.example.com/ballast", path: "ignored" })).toEqual({
      url: "rest:https://backup.example.com/ballast",
    });
  });
});

describe("needsCredentials", () => {
  it("is true for cloud/SFTP types and false for local and custom", () => {
    expect(needsCredentials("local")).toBe(false);
    expect(needsCredentials("custom")).toBe(false);
    expect(needsCredentials("b2")).toBe(true);
    expect(needsCredentials("s3")).toBe(true);
    expect(needsCredentials("r2")).toBe(true);
    expect(needsCredentials("sftp")).toBe(true);
  });
});

describe("credentialKind", () => {
  it("classifies each type's credential shape", () => {
    expect(credentialKind("b2")).toBe("keypair");
    expect(credentialKind("s3")).toBe("keypair");
    expect(credentialKind("r2")).toBe("keypair");
    expect(credentialKind("sftp")).toBe("ssh");
    expect(credentialKind("local")).toBe("none");
    expect(credentialKind("custom")).toBe("none");
  });
});

describe("typeLabel", () => {
  it("returns a friendly label for every known type", () => {
    expect(typeLabel("local")).toBe("Folder on this machine or NAS mount");
    expect(typeLabel("b2")).toBe("Backblaze B2");
    expect(typeLabel("s3")).toContain("Amazon S3");
    expect(typeLabel("r2")).toBe("Cloudflare R2");
    expect(typeLabel("sftp")).toBe("SFTP server");
    expect(typeLabel("custom")).toContain("Custom");
  });
});
