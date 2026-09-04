const { execFileSync } = require("node:child_process");

module.exports = async () => {
  execFileSync(
    "docker",
    [
      "compose",
      "-p",
      "fast-fotos-playwright",
      "-f",
      "compose.yaml",
      "-f",
      "compose.playwright.yaml",
      "down",
      "--volumes",
    ],
    { stdio: "inherit" },
  );
};
