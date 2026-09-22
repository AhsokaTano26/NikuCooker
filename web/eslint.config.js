import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import tseslint from 'typescript-eslint'

/**
 * The browser globals this application uses.
 *
 * Declared rather than pulled from the `globals` package: the list is short, it
 * is visible in one place, and a dependency that ships a thousand entries to
 * cover seventy of them is a dependency to keep updated for no gain.
 *
 * They are declared for every file rather than only for `.vue`, because the
 * event store and the API client both reach for `EventSource` and `fetch` and
 * neither is a component.
 */
const browserGlobals = {
  window: 'readonly',
  document: 'readonly',
  navigator: 'readonly',
  location: 'readonly',
  fetch: 'readonly',
  EventSource: 'readonly',
  URL: 'readonly',
  URLSearchParams: 'readonly',
  AbortController: 'readonly',
  AbortSignal: 'readonly',
  setTimeout: 'readonly',
  clearTimeout: 'readonly',
  setInterval: 'readonly',
  clearInterval: 'readonly',
  queueMicrotask: 'readonly',
  // The DOM constructors the views narrow to. Only ones actually used, so
  // the list stays short enough to read.
  HTMLTextAreaElement: 'readonly',
  HTMLInputElement: 'readonly',
  HTMLElement: 'readonly',
  confirm: 'readonly',
  alert: 'readonly',
  requestAnimationFrame: 'readonly',
  console: 'readonly',
}

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**'] },

  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...pluginVue.configs['flat/recommended'],

  {
    languageOptions: { globals: browserGlobals },
  },

  {
    files: ['**/*.vue'],
    languageOptions: {
      globals: browserGlobals,
      parserOptions: {
        // The SFC parser delegates <script lang="ts"> to the TypeScript parser.
        // Without this, type-only syntax fails to parse.
        parser: tseslint.parser,
      },
    },
  },

  {
    rules: {
      // Single-word names are the convention for App.vue and for views named
      // after the route they serve.
      'vue/multi-word-component-names': 'off',
      // Attributes are wrapped by the formatter, not policed here; this rule
      // fights with Prettier-style line breaking.
      'vue/max-attributes-per-line': 'off',
      'vue/singleline-html-element-content-newline': 'off',
      'vue/html-self-closing': 'off',

      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      // The API layer deliberately throws values it then narrows; `any` is
      // banned everywhere else by the strict tsconfig.
      '@typescript-eslint/no-explicit-any': 'error',
      eqeqeq: ['error', 'always', { null: 'ignore' }],
      'no-console': ['warn', { allow: ['warn', 'error'] }],
    },
  },

  {
    files: ['**/*.spec.ts'],
    rules: {
      // Tests reach into internals and build deliberately malformed payloads.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
)
