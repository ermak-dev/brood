function topmostDialog(): HTMLElement | null {
  const dialogs = document.querySelectorAll<HTMLElement>('[role="dialog"][aria-modal="true"]');
  return dialogs.item(dialogs.length - 1);
}

export function isTopmostDialog(dialog: HTMLElement | null): boolean {
  return dialog !== null && topmostDialog() === dialog;
}

/** Закрытие предыдущей панели не должно уводить фокус из нового редактора. */
export function restoreDialogFocus(target: HTMLElement | null): void {
  if (!target?.isConnected) return;
  const dialog = topmostDialog();
  if (!dialog || dialog.contains(target)) target.focus();
}
