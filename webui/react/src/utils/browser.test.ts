import * as utils from './browser';

describe('Browser Utilities', () => {
  describe('correctViewportHeight', () => {
    it('should correct viewport height', () => {
      // Stub out `window.innerHeight`.
      global.innerHeight = 1024;
      utils.correctViewportHeight();

      const vhProp = document.documentElement.style.getPropertyValue('--vh');
      expect(vhProp).toBe('10.24px');
    });
  });
});
