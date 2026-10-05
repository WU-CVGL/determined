import { screen } from '@testing-library/react';

/**
 * Whether the open menu shows the item with this label in red, as a dangerous action. The item is
 * matched by its text, as in menuLabels: an item's icon title would be part of its accessible name.
 */
export const isDangerMenuItem = (label: string): boolean =>
  screen
    .getByRole('menuitem', { name: (_, item) => item.textContent === label })
    .classList.contains('ant-dropdown-menu-item-danger');

/** The labels of the open menu's items, in order. */
export const menuLabels = (): (string | null)[] =>
  screen.getAllByRole('menuitem').map((item) => item.textContent);
