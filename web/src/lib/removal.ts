/**
 * What to show while a backup is being removed. Removal waits for a
 * running backup to finish first, which can take several seconds, so
 * the page says so instead of sitting silent.
 */
export function removeWaitText(name: string): string {
  return `Waiting for the running backup to finish… ${name} will be removed as soon as it has.`;
}

/** The question asked before a backup is removed; says plainly what goes and what stays. */
export function removeQuestion(name: string): string {
  return (
    `Remove "${name}" from Ballast? Ballast stops backing it up, and its schedule and notification settings are deleted. ` +
    `The backups already stored in your destinations are not touched, so you can still restore from them with restic. ` +
    `Its history here stops counting toward your status.`
  );
}

/** What to say once a backup has been removed. */
export function removedText(name: string): string {
  return `Removed ${name}. Its existing backups are still in your destinations.`;
}
