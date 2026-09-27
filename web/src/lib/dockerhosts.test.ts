import { describe, expect, it } from "vitest";

import { addressHelp, addressPlaceholder, buildCredentials, credentialKind, kindLabel } from "./dockerhosts";

describe("kindLabel", () => {
  it("returns a friendly label for every kind", () => {
    expect(kindLabel("local")).toBe("This machine");
    expect(kindLabel("remote-ssh")).toContain("SSH");
    expect(kindLabel("remote-tls")).toContain("TLS");
  });
});

describe("credentialKind", () => {
  it("classifies each kind's credential shape", () => {
    expect(credentialKind("local")).toBe("none");
    expect(credentialKind("remote-ssh")).toBe("ssh");
    expect(credentialKind("remote-tls")).toBe("tls");
  });
});

describe("addressPlaceholder / addressHelp", () => {
  it("gives SSH-shaped guidance for remote-ssh", () => {
    expect(addressPlaceholder("remote-ssh")).toContain("@");
    expect(addressHelp("remote-ssh")).toMatch(/ssh/i);
  });

  it("gives host:port guidance for remote-tls", () => {
    expect(addressPlaceholder("remote-tls")).toMatch(/^\d/);
    expect(addressHelp("remote-tls")).toMatch(/2376/);
  });

  it("explains the default socket for local", () => {
    expect(addressHelp("local")).toMatch(/socket/i);
  });
});

describe("buildCredentials", () => {
  it("returns undefined when nothing was entered", () => {
    expect(buildCredentials("remote-ssh", {})).toBeUndefined();
    expect(buildCredentials("remote-ssh", { ssh_private_key: "  " })).toBeUndefined();
    expect(buildCredentials("local", {})).toBeUndefined();
  });

  it("builds an SSH credential from the private key only", () => {
    expect(buildCredentials("remote-ssh", { ssh_private_key: " KEY ", ca_cert: "ignored" })).toEqual({
      ssh_private_key: "KEY",
    });
  });

  it("builds a TLS credential from all three PEMs", () => {
    expect(
      buildCredentials("remote-tls", { ca_cert: "CA", client_cert: "CERT", client_key: "KEY", ssh_private_key: "ignored" }),
    ).toEqual({ ca_cert: "CA", client_cert: "CERT", client_key: "KEY" });
  });

  it("keeps only the TLS fields that were filled in", () => {
    expect(buildCredentials("remote-tls", { ca_cert: "CA" })).toEqual({ ca_cert: "CA" });
  });
});
