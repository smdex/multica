// @ts-check
const { withProjectBuildGradle } = require("expo/config-plugins");

/** @param {string} contents @param {string} version */
function setAndroidNdkVersion(contents, version) {
  if (!/^\d+\.\d+\.\d+$/.test(version)) {
    throw new Error(`Invalid Android NDK version: ${version}`);
  }
  const assignment = `ext.ndkVersion = "${version}"`;
  const existing = /^ext\.ndkVersion = "[\d.]+"$/m;
  if (existing.test(contents)) return contents.replace(existing, assignment);

  const anchor = 'apply plugin: "expo-root-project"';
  if (!contents.includes(anchor)) {
    throw new Error("Cannot configure Android NDK: Expo root project plugin is missing");
  }
  return contents.replace(anchor, `${assignment}\n\n${anchor}`);
}

// Expo's root plugin otherwise selects React Native's default NDK. AndroidMath's
// newer fbjni requires the C++ runtime from NDK 28 across all native modules.
/** @type {import("expo/config-plugins").ConfigPlugin<{ version: string }>} */
const withAndroidNdk = (config, { version }) =>
  withProjectBuildGradle(config, (config) => {
    config.modResults.contents = setAndroidNdkVersion(config.modResults.contents, version);
    return config;
  });

module.exports = withAndroidNdk;
module.exports.setAndroidNdkVersion = setAndroidNdkVersion;
