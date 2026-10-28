import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "ARTEX",
  version: packageJson.version,
  copyright: `© ${currentYear}, ARTEX.`,
  meta: {
    title: "ARTEX — Plataforma autônoma de testes de intrusão",
    description: "Plataforma corporativa de testes de intrusão orientada por LLM",
  },
};
