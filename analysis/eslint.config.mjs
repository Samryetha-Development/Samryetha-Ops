import tseslint from "typescript-eslint";
import security from "eslint-plugin-security";
import reactHooks from "eslint-plugin-react-hooks";

const FILES = ["backend/src/**/*.ts", "frontend/src/**/*.ts", "frontend/src/**/*.tsx"];

export default tseslint.config(
  { ignores: ["**/dist/**", "**/node_modules/**", "**/*.test.ts"] },
  {
    files: FILES,
    linterOptions: { reportUnusedDisableDirectives: "off" },
    extends: [...tseslint.configs.recommendedTypeChecked],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: { security, "react-hooks": reactHooks },
    rules: {
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-unused-vars": "off",

      "@typescript-eslint/no-confusing-void-expression": "off",
      "@typescript-eslint/no-misused-promises": "off",
      "@typescript-eslint/no-unnecessary-condition": "off",
      "@typescript-eslint/no-redundant-type-constituents": "off",
      "@typescript-eslint/no-unsafe-argument": "off",
      "@typescript-eslint/prefer-nullish-coalescing": "off",
      "@typescript-eslint/no-non-null-assertion": "off",
      "no-await-in-loop": "off",
      "no-empty": "off",
      "eqeqeq": "off",
      "security/detect-possible-timing-attacks": "off",
      "react-hooks/exhaustive-deps": "off",

      "@typescript-eslint/await-thenable": "error",
      "@typescript-eslint/require-await": "error",
      "@typescript-eslint/no-base-to-string": "error",
      "@typescript-eslint/no-mixed-enums": "error",
      "@typescript-eslint/no-unnecessary-type-assertion": "warn",
      "@typescript-eslint/switch-exhaustiveness-check": "warn",
      "@typescript-eslint/prefer-optional-chain": "warn",
      "require-atomic-updates": "error",
      "no-async-promise-executor": "error",
      "no-promise-executor-return": "error",
      "no-fallthrough": "error",
      "no-constant-binary-expression": "error",
      "no-constructor-return": "error",
      "no-self-compare": "error",
      "no-unmodified-loop-condition": "error",
      "no-unreachable-loop": "warn",
      "prefer-promise-reject-errors": "error",
      "security/detect-child-process": "warn",
      "security/detect-eval-with-expression": "error",
      "security/detect-non-literal-require": "warn",
      "security/detect-object-injection": "off",
      "security/detect-non-literal-fs-filename": "off",
      "security/detect-pseudoRandomBytes": "warn",
      "security/detect-new-buffer": "error",
    },
  },
);
