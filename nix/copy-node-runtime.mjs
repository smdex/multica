import { cpSync, existsSync, mkdirSync, readFileSync, realpathSync } from "node:fs";
import { createRequire } from "node:module";
import { basename, dirname, join, parse } from "node:path";

const [sourcePackageJson, targetNodeModules, ...rootPackages] = process.argv.slice(2);

if (!sourcePackageJson || !targetNodeModules || rootPackages.length === 0) {
  throw new Error(
    "usage: copy-node-runtime.mjs <source package.json> <target node_modules> <package>...",
  );
}

function packageRootFromEntry(entry) {
  let current = dirname(realpathSync(entry));
  const filesystemRoot = parse(current).root;

  while (current !== filesystemRoot) {
    if (existsSync(join(current, "package.json"))) return current;
    current = dirname(current);
  }

  throw new Error(`could not find package.json above ${entry}`);
}

function resolvePackageRoot(requireFrom, packageName) {
  try {
    return dirname(realpathSync(requireFrom.resolve(`${packageName}/package.json`)));
  } catch {
    return packageRootFromEntry(requireFrom.resolve(packageName));
  }
}

function packageTarget(nodeModules, packageName) {
  return join(nodeModules, ...packageName.split("/"));
}

function copyPackage(packageName, requireFrom, nodeModules, ancestry = []) {
  const sourceRoot = resolvePackageRoot(requireFrom, packageName);
  const packageJson = JSON.parse(readFileSync(join(sourceRoot, "package.json"), "utf8"));
  const targetRoot = packageTarget(nodeModules, packageName);

  if (existsSync(join(targetRoot, "package.json"))) {
    const existing = JSON.parse(readFileSync(join(targetRoot, "package.json"), "utf8"));
    if (existing.name === packageJson.name && existing.version === packageJson.version) return;
    throw new Error(
      `runtime dependency collision at ${targetRoot}: ` +
        `${existing.name}@${existing.version} vs ${packageJson.name}@${packageJson.version}`,
    );
  }

  mkdirSync(dirname(targetRoot), { recursive: true });
  cpSync(sourceRoot, targetRoot, {
    recursive: true,
    dereference: true,
    filter: (source) => source === sourceRoot || basename(source) !== "node_modules",
  });

  const dependencies = {
    ...(packageJson.dependencies ?? {}),
    ...(packageJson.optionalDependencies ?? {}),
  };
  const childRequire = createRequire(join(sourceRoot, "package.json"));

  for (const dependencyName of Object.keys(dependencies).sort()) {
    try {
      copyPackage(
        dependencyName,
        childRequire,
        join(targetRoot, "node_modules"),
        [...ancestry, packageName],
      );
    } catch (error) {
      if (packageJson.optionalDependencies?.[dependencyName]) continue;
      throw new Error(
        `while copying ${[...ancestry, packageName, dependencyName].join(" -> ")}: ${error.message}`,
        { cause: error },
      );
    }
  }
}

mkdirSync(targetNodeModules, { recursive: true });
const rootRequire = createRequire(sourcePackageJson);
for (const packageName of rootPackages) {
  copyPackage(packageName, rootRequire, targetNodeModules);
}
