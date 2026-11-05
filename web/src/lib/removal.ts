/**
 * What to show while a backup is being removed. Removal waits for a
 * running backup to finish first, which can take several seconds, so
 * the page says so instead of sitting silent.
 */
export function removeWaitText(name: string): string {
  return `Waiting for the running backup to finish… ${name} will be removed as soon as it has.`;
}
