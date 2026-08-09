import { Switch as SwitchPrimitive } from "@base-ui/react/switch"

import { cn } from "@/lib/utils"

// shadcn base 版 Switch（@base-ui/react/switch，与项目其他组件同基座）。
// 受控：checked + onCheckedChange；非受控：defaultChecked。
function Switch({
  className,
  ...props
}: SwitchPrimitive.Root.Props) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer inline-flex h-5 w-9 shrink-0 items-center rounded-full border border-transparent bg-input shadow-xs outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 data-checked:bg-primary data-disabled:cursor-not-allowed data-disabled:opacity-50 dark:bg-input/30 dark:data-checked:bg-primary",
        className
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className={cn(
          "pointer-events-none block size-4 translate-x-0 rounded-full bg-background shadow-sm ring-0 transition-transform data-checked:translate-x-4"
        )}
      />
    </SwitchPrimitive.Root>
  )
}

export { Switch }
