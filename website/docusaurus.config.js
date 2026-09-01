// @ts-check
// Docs-only site: the markdown lives in the repo's top-level docs/ directory and
// is consumed from there, so every page stays readable as a plain file on GitHub.

const config = {
  title: 'oh-my-agentic-coder',
  tagline: 'Run an agentic coding harness inside an OS sandbox',
  favicon: 'img/favicon.svg',

  // Published at https://tng.github.io/oh-my-agentic-coder/. A fork of this repo
  // serves the same baseUrl (the path is the repo name), so a fork's Pages site
  // works with this config unchanged.
  url: 'https://tng.github.io',
  baseUrl: '/oh-my-agentic-coder/',
  organizationName: 'TNG',
  projectName: 'oh-my-agentic-coder',

  // A dead link is a build failure, so CI catches doc drift instead of shipping it.
  onBrokenLinks: 'throw',

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },
  themes: ['@docusaurus/theme-mermaid'],

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs',
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl: 'https://github.com/TNG/oh-my-agentic-coder/tree/main/docs/',
          showLastUpdateTime: true,
        },
        blog: false,
        pages: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      },
    ],
  ],

  themeConfig: {
    navbar: {
      title: 'omac',
      items: [
        { type: 'docSidebar', sidebarId: 'docs', position: 'left', label: 'Docs' },
        {
          href: 'https://github.com/TNG/oh-my-agentic-coder',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            { label: 'Quick start', to: '/getting-started/quick-start' },
            { label: 'CLI reference', to: '/usage/cli' },
            { label: 'Security model', to: '/security' },
          ],
        },
        {
          title: 'Project',
          items: [
            { label: 'GitHub', href: 'https://github.com/TNG/oh-my-agentic-coder' },
            { label: 'Issues', href: 'https://github.com/TNG/oh-my-agentic-coder/issues' },
            { label: 'Releases', href: 'https://github.com/TNG/oh-my-agentic-coder/releases' },
          ],
        },
      ],
      copyright: 'Copyright © 2026 TNG Technology Consulting GmbH. Apache License 2.0.',
    },
    prism: {
      additionalLanguages: ['bash', 'json', 'yaml', 'go', 'toml'],
    },
  },
};

export default config;
