import { defineConfig } from '@rsbuild/core';
import { pluginReact } from '@rsbuild/plugin-react';

// 面板前端工程：构建产物输出到 internal/panel/dist，由 Go go:embed 打进二进制。
// assetPrefix=/panel/ 使产物引用 /panel/static/*
export default defineConfig({
  plugins: [pluginReact()],
  source: {
    entry: { index: './src/main.tsx' },
  },
  resolve: {
    alias: { '@': './src' },
  },
  html: {
    template: './index.html',
  },
  output: {
    distPath: { root: '../internal/panel/dist' },
    assetPrefix: '/panel/',
    cleanDistPath: true,
    // 禁用内联脚本/样式：严格 CSP（script-src 'self'）下内联会被拦截。
    inlineScripts: false,
    inlineStyles: false,
  },
});
