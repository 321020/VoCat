import { useEffect, type ReactNode } from "react";
import { DismissRegular } from "@fluentui/react-icons";
import { cx } from "../../lib/utils";
import { useI18n } from "../../lib/i18n";

export interface ModalProps {
  open: boolean;
  onClose: () => void;
  title?: ReactNode;
  width?: string;
  children: ReactNode;
  footer?: ReactNode;
  showClose?: boolean;
  closeOnOverlay?: boolean;
  className?: string;
  bodyClassName?: string;
}

// Glassmorphism modal replicating VoHive's `.el-dialog.glass-modal`.
export function Modal({
  open,
  onClose,
  title,
  width = "max-w-lg",
  children,
  footer,
  showClose = true,
  closeOnOverlay = true,
  className,
  bodyClassName,
}: ModalProps) {
  const { t } = useI18n();
  useEffect(() => {
    if (!open) return;
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-[3000] flex items-center justify-center overflow-y-auto bg-black/50 p-4 backdrop-blur-sm animate-[fade-slide-in_0.2s_ease]"
      onMouseDown={(event) => {
        if (closeOnOverlay && event.target === event.currentTarget) onClose();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        className={cx(
          "glass-modal relative flex max-h-[calc(100dvh-2rem)] w-full flex-col rounded-2xl shadow-2xl animate-[fade-slide-in_0.25s_cubic-bezier(0.4,0,0.2,1)]",
          width,
          className,
        )}
      >
        {(title || showClose) && (
          <div className="flex shrink-0 items-center justify-between px-6 pt-5 pb-3">
            <div className="text-base font-bold text-gray-900 dark:text-white">{title}</div>
            {showClose && (
              <button
                type="button"
                onClick={onClose}
                aria-label={t("关闭")}
                className="rounded-md p-1 text-gray-400 transition-colors hover:bg-black/5 hover:text-gray-600 dark:hover:bg-white/10 dark:hover:text-gray-200"
              >
                <DismissRegular className="text-[18px]" />
              </button>
            )}
          </div>
        )}
        <div className={cx("min-h-0 flex-1 overflow-y-auto px-6 pb-5", !title && "pt-5", bodyClassName)}>{children}</div>
        {footer && <div className="flex shrink-0 items-center justify-end gap-3 px-6 pb-5">{footer}</div>}
      </div>
    </div>
  );
}
