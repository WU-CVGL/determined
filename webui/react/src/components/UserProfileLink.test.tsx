import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import UserProfileLink from './UserProfileLink';

const setup = (children: React.ReactNode = 'bob') => {
  const onRowClick = vi.fn();
  render(
    <UIProvider theme={DefaultTheme.Light}>
      {/* A table row, as in the Jobs list. */}
      <div onClick={onRowClick}>
        <UserProfileLink name="bob" userId={4}>
          {children}
        </UserProfileLink>
      </div>
    </UIProvider>,
  );
  return { onRowClick };
};

describe('UserProfileLink', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/');
    vi.restoreAllMocks();
  });

  it("opens the user's page without a click on what is around it", async () => {
    const user = userEvent.setup();
    const { onRowClick } = setup();
    const link = screen.getByRole('link', { name: 'bob' });
    expect(link).toHaveAttribute('href', '/users/4');
    expect(link).toHaveTextContent('bob');
    await user.click(link);
    expect(window.location.pathname).toBe('/users/4');
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it('is named by the user when it shows something else', () => {
    setup(<span>BO</span>);
    expect(screen.getByRole('link', { name: 'bob' })).toHaveTextContent(/^BO$/);
  });

  it.each(['Control', 'Meta'])('opens the page in a new tab on a click with %s', async (key) => {
    const user = userEvent.setup();
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const { onRowClick } = setup();
    await user.keyboard(`{${key}>}`);
    await user.click(screen.getByRole('link', { name: 'bob' }));
    await user.keyboard(`{/${key}}`);
    expect(open).toHaveBeenCalledWith('/users/4', '_blank', expect.any(String));
    expect(window.location.pathname).toBe('/');
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it('leaves a middle click to the browser', async () => {
    const user = userEvent.setup();
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const { onRowClick } = setup();
    await user.pointer({
      keys: '[MouseMiddle]',
      target: screen.getByRole('link', { name: 'bob' }),
    });
    expect(open).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe('/');
    expect(onRowClick).not.toHaveBeenCalled();
  });
});
