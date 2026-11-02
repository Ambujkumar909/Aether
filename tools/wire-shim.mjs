// Re-export the browser codec so tools can use the exact same encoder the
// product ships. Importing across the directory boundary in one place keeps
// every tool from hard-coding a relative path.
export * from '../web/src/wire.js';
