import { screen } from '@testing-library/react';

/** Whether the open menu shows the item with this label in red, as a dangerous action. */
export const isDangerMenuItem = (label: string): boolean =>
  screen.getByRole('menuitem', { name: label }).classList.contains('ant-dropdown-menu-item-danger');

/** The labels of the open menu's items, in order. */
export const menuLabels = (): (string | null)[] =>
  screen.getAllByRole('menuitem').map((item) => item.textContent);
