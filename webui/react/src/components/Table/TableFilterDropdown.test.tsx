import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';

import TableFilterDropdown, { ARIA_LABEL_APPLY } from './TableFilterDropdown';

const user = userEvent.setup();

const OPTIONS = ['Alpha', 'Beta', 'Gamma', 'Delta'].map((text) => ({
  text,
  value: text.toLowerCase(),
}));

const setup = (props: Partial<React.ComponentProps<typeof TableFilterDropdown>> = {}) => {
  const handlers = { close: vi.fn(), confirm: vi.fn(), onFilter: vi.fn() };
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <TableFilterDropdown
        checklist
        filters={OPTIONS}
        multiple
        prefixCls="ant-dropdown-custom"
        selectedKeys={[]}
        setSelectedKeys={vi.fn()}
        values={[]}
        visible
        {...handlers}
        {...props}
      />
    </UIProvider>,
  );
  return handlers;
};

const ticked = () =>
  screen
    .getAllByRole('option')
    .filter((option) => option.getAttribute('aria-selected') === 'true')
    .map((option) => option.textContent);

describe('TableFilterDropdown', () => {
  describe('as a tick list', () => {
    it('ticks the shown options with All and unticks them with None', async () => {
      const { onFilter } = setup({ searchable: true, values: ['delta'] });

      await user.type(screen.getByPlaceholderText('search filters'), 'mm');
      expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual(['Gamma']);
      await user.click(screen.getByRole('button', { name: 'All' }));
      await user.clear(screen.getByPlaceholderText('search filters'));
      expect(ticked()).toEqual(['Gamma', 'Delta']);

      await user.type(screen.getByPlaceholderText('search filters'), 'delta');
      await user.click(screen.getByRole('button', { name: 'None' }));
      await user.click(screen.getByRole('button', { name: 'OK' }));

      expect(onFilter).toHaveBeenCalledWith(['gamma']);
    });

    it('applies no filter with every option or none ticked', async () => {
      const { confirm, onFilter } = setup({ values: ['beta'] });

      await user.click(screen.getByRole('button', { name: 'All' }));
      await user.click(screen.getByRole('button', { name: 'OK' }));
      expect(onFilter).toHaveBeenLastCalledWith([]);

      await user.click(screen.getByRole('button', { name: 'None' }));
      await user.click(screen.getByRole('button', { name: 'OK' }));
      expect(onFilter).toHaveBeenLastCalledWith([]);
      expect(confirm).toHaveBeenCalledTimes(2);
    });

    it('names the list, and shows each option in full on hover', () => {
      setup({ label: 'Owner' });

      expect(screen.getByRole('listbox', { name: 'Owner' })).toBeInTheDocument();
      // Not on the option's text, which takes no pointer events.
      expect(screen.getByRole('option', { name: 'Gamma' })).toHaveAttribute('title', 'Gamma');
    });

    it('ticks every option but the one of a Ctrl+click or a Cmd+click', async () => {
      setup();

      await user.keyboard('{Control>}');
      await user.click(screen.getByRole('option', { name: 'Beta' }));
      await user.keyboard('{/Control}');
      expect(ticked()).toEqual(['Alpha', 'Gamma', 'Delta']);

      await user.click(screen.getByRole('button', { name: 'None' }));
      await user.keyboard('{Meta>}');
      await user.click(screen.getByRole('option', { name: 'Gamma' }));
      await user.keyboard('{/Meta}');
      expect(ticked()).toEqual(['Alpha', 'Beta', 'Delta']);
    });

    it('is one tab stop: Up and Down move, Space ticks, Enter applies', async () => {
      const { onFilter } = setup();
      const list = screen.getByRole('listbox');
      await waitFor(() => expect(list).toHaveFocus());

      await user.keyboard('{ArrowDown}{ArrowDown}{ }{ArrowUp}{ }');
      expect(ticked()).toEqual(['Beta', 'Gamma']);
      expect(list).toHaveAttribute(
        'aria-activedescendant',
        screen.getByText('Beta').closest('[role="option"]')?.id,
      );
      await user.keyboard('{Enter}');

      expect(onFilter).toHaveBeenCalledWith(['beta', 'gamma']);
    });

    it('goes round the search, the list, All, None and OK with Tab', async () => {
      setup({ searchable: true });
      const search = screen.getByPlaceholderText('search filters');
      await waitFor(() => expect(search).toHaveFocus());

      const order = [
        screen.getByRole('listbox'),
        screen.getByRole('button', { name: 'All' }),
        screen.getByRole('button', { name: 'None' }),
        screen.getByRole('button', { name: 'OK' }),
        search,
      ];
      for (const stop of order) {
        await user.tab();
        expect(stop).toHaveFocus();
      }
      await user.tab({ shift: true });
      expect(screen.getByRole('button', { name: 'OK' })).toHaveFocus();
    });

    it('closes on Escape without applying', async () => {
      const { close, confirm, onFilter } = setup();
      await user.click(screen.getByRole('option', { name: 'Alpha' }));

      await user.keyboard('{Escape}');

      expect(close).toHaveBeenCalled();
      expect(confirm).not.toHaveBeenCalled();
      expect(onFilter).not.toHaveBeenCalled();
    });

    it.each([
      [10, '280px'],
      [14, '336px'],
    ])('shows up to 12 options without a search box: %i options are %s high', (count, height) => {
      setup({
        filters: Array.from({ length: count }, (_, i) => ({ text: String(i), value: String(i) })),
      });
      expect((screen.getByRole('listbox').firstChild as HTMLElement).style.height).toBe(height);
      expect(within(screen.getByRole('listbox')).getByText('9')).toBeInTheDocument();
    });
  });

  it('keeps Reset and an OK button labelled for its tests elsewhere', () => {
    setup({ checklist: false });
    expect(screen.getByRole('button', { name: 'table-filter-reset' })).toBeInTheDocument();
    expect(screen.getByLabelText(ARIA_LABEL_APPLY)).toHaveTextContent('OK');
    expect(screen.queryByRole('button', { name: 'All' })).not.toBeInTheDocument();
  });
});
