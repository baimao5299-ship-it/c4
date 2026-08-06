import * as React from "react"
import { Button as ButtonPrimitive } from "@base-ui/react/button"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const buttonVariants = cva(
  "group/button inline-flex shrink-0 items-center justify-center rounded-lg border border-transparent bg-clip-padding text-sm font-medium whitespace-nowrap transition-all outline-none select-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 active:not-aria-[haspopup]:translate-y-px disabled:pointer-events-none disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4 [&_svg]:transition-transform [&_svg]:duration-150 hover:[&_svg]:scale-110",
  {
    variants: {
      variant: {
        /* 通透玻璃（GooseHyperGlass transparent style）：半透明白 + 顶白高光 + 白细边 + 软投影，无实心填充；
           强调 = #0088FF（gooseLight accent），仅文字/细边/光晕 */
        default:
          "border-white/50 bg-white/45 text-primary shadow-[inset_0_1px_3px_rgba(255,255,255,0.4),0_2px_10px_rgba(0,0,0,0.08)] backdrop-blur-md hover:bg-white/70 hover:shadow-[inset_0_1px_3px_rgba(255,255,255,0.5),0_4px_18px_rgba(0,136,255,0.3)]",
        outline:
          "border-white/50 bg-white/35 text-foreground shadow-[inset_0_1px_3px_rgba(255,255,255,0.35),0_2px_10px_rgba(0,0,0,0.06)] backdrop-blur-md hover:bg-white/60 hover:text-primary hover:shadow-[inset_0_1px_3px_rgba(255,255,255,0.45),0_4px_18px_rgba(0,136,255,0.25)]",
        secondary:
          "border-white/50 bg-white/35 text-secondary-foreground shadow-[inset_0_1px_3px_rgba(255,255,255,0.35),0_2px_10px_rgba(0,0,0,0.06)] backdrop-blur-md hover:bg-white/60 hover:text-primary hover:shadow-[inset_0_1px_3px_rgba(255,255,255,0.45),0_4px_18px_rgba(0,136,255,0.25)]",
        ghost:
          "hover:bg-white/45 hover:text-primary aria-expanded:bg-white/45 aria-expanded:text-primary",
        destructive:
          "border-white/50 bg-white/40 text-red-600 shadow-[inset_0_1px_3px_rgba(255,255,255,0.35),0_2px_10px_rgba(0,0,0,0.06)] backdrop-blur-md hover:bg-red-500/15 hover:shadow-[inset_0_1px_3px_rgba(255,255,255,0.45),0_4px_18px_rgba(244,63,94,0.2)] focus-visible:border-red-400/40 focus-visible:ring-red-500/20",
        link: "text-primary underline-offset-4 hover:underline",
      },
      size: {
        default:
          "h-8 gap-1.5 px-2.5 has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2",
        xs: "h-6 gap-1 rounded-[min(var(--radius-md),10px)] px-2 text-xs in-data-[slot=button-group]:rounded-lg has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: "h-7 gap-1 rounded-[min(var(--radius-md),12px)] px-2.5 text-[0.8rem] in-data-[slot=button-group]:rounded-lg has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3.5",
        lg: "h-9 gap-1.5 px-2.5 has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2",
        icon: "size-8",
        "icon-xs":
          "size-6 rounded-[min(var(--radius-md),10px)] in-data-[slot=button-group]:rounded-lg [&_svg:not([class*='size-'])]:size-3",
        "icon-sm":
          "size-7 rounded-[min(var(--radius-md),12px)] in-data-[slot=button-group]:rounded-lg",
        "icon-lg": "size-9",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

const Button = React.forwardRef<
  HTMLButtonElement,
  ButtonPrimitive.Props & VariantProps<typeof buttonVariants>
>(function Button(
  { className, variant = "default", size = "default", ...props },
  ref
) {
  return (
    <ButtonPrimitive
      ref={ref}
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
})

Button.displayName = "Button"

export { Button, buttonVariants }
