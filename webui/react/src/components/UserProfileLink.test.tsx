import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import UserProfileLink from './UserProfileLink';

describe('UserProfileLink', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/');
  });

  it("opens the user's page without a click on what is around it", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    render(
      <UIProvider theme={DefaultTheme.Light}>
        {/* A table row, as in the Jobs list. */}
        <div onClick={onRowClick}>
          <UserProfileLink userId={4}>bob</UserProfileLink>
        </div>
      </UIProvider>,
    );
    const link = screen.getByText('bob').closest('a') as HTMLElement;
    expect(link).toHaveAttribute('href', '/users/4');
    await user.click(link);
    expect(window.location.pathname).toBe('/users/4');
    expect(onRowClick).not.toHaveBeenCalled();
  });
});
