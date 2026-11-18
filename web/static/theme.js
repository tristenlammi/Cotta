// Applies a saved light/dark choice before the page paints, so there is no
// flash of the wrong theme. With no saved choice the system setting decides.
(function () {
  try {
    var t = localStorage.getItem('cotta-theme');
    if (t === 'dark' || t === 'light') document.documentElement.dataset.theme = t;
  } catch (e) { /* storage unavailable: follow the system */ }
})();
