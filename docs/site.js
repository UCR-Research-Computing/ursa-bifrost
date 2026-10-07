// Copy-to-clipboard for install commands.
document.querySelectorAll('button.copy').forEach(function (btn) {
  btn.addEventListener('click', function () {
    var el = document.getElementById(btn.getAttribute('data-copy'));
    if (!el || !navigator.clipboard) return;
    navigator.clipboard.writeText(el.textContent).then(function () {
      btn.classList.add('done');
      btn.innerHTML = '<i class="fa-solid fa-check" aria-hidden="true"></i>';
      setTimeout(function () {
        btn.classList.remove('done');
        btn.innerHTML = '<i class="fa-regular fa-copy" aria-hidden="true"></i>';
      }, 1600);
    });
  });
});
