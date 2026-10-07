// @vitest-environment node
import { describe, expect, it } from "vitest";
import { setAndroidNdkVersion } from "./with-android-ndk";

const source = 'buildscript { }\n\napply plugin: "expo-root-project"\n';

describe("setAndroidNdkVersion", () => {
  it("sets the NDK before Expo selects its default", () => {
    expect(setAndroidNdkVersion(source, "28.2.13676358")).toBe(
      'buildscript { }\n\next.ndkVersion = "28.2.13676358"\n\napply plugin: "expo-root-project"\n',
    );
  });

  it("is idempotent and updates an existing version", () => {
    const patched = setAndroidNdkVersion(source, "28.2.13676358");
    expect(setAndroidNdkVersion(patched, "28.2.13676358")).toBe(patched);
    expect(setAndroidNdkVersion(patched, "28.3.12345678")).toBe(
      patched.replace("28.2.13676358", "28.3.12345678"),
    );
  });

  it("rejects an unexpected template or invalid version", () => {
    expect(() => setAndroidNdkVersion("buildscript { }", "28.2.13676358")).toThrow(
      "Expo root project plugin is missing",
    );
    expect(() => setAndroidNdkVersion(source, '28"')).toThrow("Invalid Android NDK version");
  });
});
