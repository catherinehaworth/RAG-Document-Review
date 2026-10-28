document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll('input[type="file"]').forEach((input) => {
    input.addEventListener("change", () => {
      const file = input.files && input.files[0];
      if (!file) return;
      const form = input.closest("form");
      const status = form && form.querySelector(".form-status");
      if (status) status.textContent = `${file.name} · ${formatFileSize(file.size)}`;
    });
  });

  document.querySelectorAll("form[data-confirm]").forEach((form) => {
    form.addEventListener("submit", (event) => {
      if (!window.confirm(form.dataset.confirm)) event.preventDefault();
    });
  });

  document.querySelectorAll("form[data-busy-message]").forEach((form) => {
    form.addEventListener("submit", (event) => {
      const fileInputs = form.querySelectorAll('input[type="file"]');
      for (const input of fileInputs) {
        if ((input.required || input.dataset.requiredFile === "true") && (!input.files || input.files.length === 0)) {
          event.preventDefault();
          const status = form.querySelector(".form-status");
          if (status) status.textContent = "Choose a supported file before continuing.";
          return;
        }
        if (!input.files || input.files.length === 0) continue;
        const maxBytes = Number(input.dataset.maxBytes || 0);
        if (maxBytes > 0 && input.files[0].size > maxBytes) {
          event.preventDefault();
          const status = form.querySelector(".form-status");
          if (status) status.textContent = `This file is too large. Maximum size is ${formatFileSize(maxBytes)}.`;
          return;
        }
      }

      const submit = form.querySelector('button[type="submit"]');
      const status = form.querySelector(".form-status");
      if (submit) {
        submit.disabled = true;
        submit.setAttribute("aria-busy", "true");
      }
      if (status) {
        status.textContent = form.dataset.busyMessage;
        status.classList.add("is-busy");
      }
    });

    const searchAll = document.querySelector('input[name="search_all"]');
    const selectedFiles = document.querySelectorAll('input[name="file_ids"]');
    selectedFiles.forEach((input) => {
      input.addEventListener("change", () => {
        if (input.checked && searchAll) searchAll.checked = false;
      });
    });
    if (searchAll) {
      searchAll.addEventListener("change", () => {
        if (searchAll.checked) selectedFiles.forEach((input) => { input.checked = false; });
      });
    }
    const fileFilter = document.querySelector("#file-filter");
    if (fileFilter) {
      fileFilter.addEventListener("input", () => {
        const query = fileFilter.value.trim().toLowerCase();
        document.querySelectorAll(".file-choice").forEach((label) => {
          label.hidden = !label.textContent.toLowerCase().includes(query);
        });
      });
    }
  });

  const upload = document.querySelector("#upload-file");
  if (upload) {
    const zone = upload.closest(".upload-zone");
    ["dragenter", "dragover"].forEach((eventName) => {
      zone.addEventListener(eventName, (event) => {
        event.preventDefault();
        zone.classList.add("is-dragging");
      });
    });
    ["dragleave", "drop"].forEach((eventName) => {
      zone.addEventListener(eventName, (event) => {
        event.preventDefault();
        zone.classList.remove("is-dragging");
      });
    });
    zone.addEventListener("drop", (event) => {
      if (!event.dataTransfer || !event.dataTransfer.files.length) return;
      upload.files = event.dataTransfer.files;
      upload.dispatchEvent(new Event("change", { bubbles: true }));
    });
  }
});

function formatFileSize(size) {
  if (size < 1024) return `${size} B`;
  const units = ["KB", "MB", "GB"];
  let value = size;
  for (const unit of units) {
    value /= 1024;
    if (value < 1024) return `${value.toFixed(1)} ${unit}`;
  }
  return `${value.toFixed(1)} TB`;
}
