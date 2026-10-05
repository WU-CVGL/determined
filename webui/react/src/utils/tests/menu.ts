import { screen } from '@testing-library/react';

/*
 * The items of the open menus. A closed antd menu stays in the page with the ant-dropdown-hidden
 * class, which jsdom does not turn into display: none, so the role query alone finds its items too.
 */
const openMenuItems = (): HTMLElement[] =>
  screen.getAllByRole('menuitem').filter((item) => !item.closest('.ant-dropdown-hidden'));

/**
 * The open menu's item with this label. The item is matched by its text, as in menuLabels: an
 * item's icon title would be part of its accessible name.
 */
export const openMenuItem = (label: string): HTMLElement => {
  const items = openMenuItems().filter((item) => item.textContent === label);
  if (items.length !== 1) {
    throw new Error(`Expected one open menu item "${label}", found ${items.length}.`);
  }
  return items[0];
};

/** Whether the open menu shows the item with this label in red, as a dangerous action. */
export const isDangerMenuItem = (label: string): boolean =>
  openMenuItem(label).classList.contains('ant-dropdown-menu-item-danger');

/** Whether the open menu shows the item with this label disabled. */
export const isDisabledMenuItem = (label: string): boolean =>
  openMenuItem(label).getAttribute('aria-disabled') === 'true';

/** The labels of the open menu's items, in order. */
export const menuLabels = (): (string | null)[] => openMenuItems().map((item) => item.textContent);
