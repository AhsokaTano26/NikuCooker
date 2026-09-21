import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import tseslint from 'typescript-eslint'

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**'] },

  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...pluginVue.configs['flat/recommended'],

  {
    files: ['**/*.vue'],
    languageOptions: {
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
