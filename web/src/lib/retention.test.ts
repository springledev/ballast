import { describe, expect, it } from "vitest";

import { describeRetention, describeServiceRetention } from "./retention";

describe("describeRetention", () => {
  it("lists all three units", () => {
    expect(describeRetention({ keep_daily: 7, keep_weekly: 4, keep_monthly: 6 })).toBe(
      "7 daily, 4 weekly and 6 monthly backups",
    );
  });

  it("uses singular units for a count of 1", () => {
    expect(describeRetention({ keep_daily: 1, keep_weekly: 1, keep_monthly: 1 })).toBe(
      "1 daily, 1 weekly and 1 monthly backups",
    );
  });

  it("drops units with a zero count", () => {
    expect(describeRetention({ keep_daily: 7, keep_weekly: 0, keep_monthly: 0 })).toBe("7 daily backups");
    expect(describeRetention({ keep_daily: 7, keep_weekly: 4, keep_monthly: 0 })).toBe("7 daily and 4 weekly backups");
  });

  it("reports an all-zero policy", () => {
    expect(describeRetention({ keep_daily: 0, keep_weekly: 0, keep_monthly: 0 })).toBe("no backups kept");
  });
});

describe("describeServiceRetention", () => {
  it("names the destination when inherited", () => {
    expect(describeServiceRetention({ keep_daily: 7, keep_weekly: 4, keep_monthly: 6 }, "nas", false)).toBe(
      "Keeps 7 daily, 4 weekly and 6 monthly backups (same as nas)",
    );
  });

  it("says it's the service's own setting when overridden", () => {
    expect(describeServiceRetention({ keep_daily: 3, keep_weekly: 0, keep_monthly: 0 }, "nas", true)).toBe(
      "Keeps 3 daily backups (its own setting, not nas's)",
    );
  });
});

describe("describeRetention keep_last", () => {
  it("leads with the newest N", () => {
    expect(describeRetention({ keep_last: 2, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 })).toBe("the newest 2 backups");
    expect(describeRetention({ keep_last: 2, keep_daily: 7, keep_weekly: 4, keep_monthly: 6 })).toBe(
      "the newest 2, 7 daily, 4 weekly and 6 monthly backups",
    );
  });
});
