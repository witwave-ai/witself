// Progressive enhancement only: all product content is already in the HTML.
for (const button of document.querySelectorAll('[data-copy]')) {
  button.hidden = false;
  button.addEventListener('click', async () => {
    const code = document.getElementById(button.dataset.copy);
    try {
      await navigator.clipboard.writeText(code.textContent);
      button.textContent = 'Copied';
      document.getElementById('copy-status').textContent = 'Install command copied.';
    } catch {
      button.textContent = 'Select command';
      const range = document.createRange();
      range.selectNodeContents(code);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      document.getElementById('copy-status').textContent = 'Copy is unavailable. The command is selected for manual copying.';
    }
    window.setTimeout(() => { button.textContent = 'Copy'; }, 2500);
  });
}
