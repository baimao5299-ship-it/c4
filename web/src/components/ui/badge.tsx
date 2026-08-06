import { mergeProps } from "@base-ui/react/merge-props"
import { useRender } from "@base-ui/react/use-render"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "group/badge inline-flex h-5 w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-4xl border border-transparent px-2 py-0.5 text-xs font-medium whitespace-nowrap transition-all focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 aria-invalid:border-destructive aria-invalid:ring-destructive/20 [&>svg]:pointer-events-none [&>svg]:size-3!",
  {
    variants: {
      variant: {
        /* 通透玻璃标签：白细边 + 顶白高光，indigo 仅作文字/描边强调 */
        default:
          "border-indigo-200/60 bg-white/45 text-indigo-700 shadow-[inset_0_1px_0_rgba(255,255,255,0.7)] backdrop-blur-md [a]:hover:bg-white/65",
        secondary:
          "border-white/50 bg-white/45 text-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.65)] backdrop-blur-md [a]:hover:bg-white/65",
        destructive:
          "border-white/50 bg-white/45 text-red-600 shadow-[inset_0_1px_0_rgba(255,255,255,0.65)] backdrop-blur-md focus-visible:ring-red-500/20 [a]:hover:bg-red-500/10",
        outline:
          "border-indigo-200/60 bg-white/30 text-foreground backdrop-blur-md [a]:hover:bg-white/55 [a]:hover:text-indigo-700",
        ghost:
          "hover:bg-white/45 hover:text-indigo-700",
        link: "text-indigo-600 underline-offset-4 hover:underline",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Badge({
  className,
  variant = "default",
  render,
  ...props
}: useRender.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return useRender({
    defaultTagName: "span",
    props: mergeProps<"span">(
      {
        className: cn(badgeVariants({ variant }), className),
      },
      props
    ),
    render,
    state: {
      slot: "badge",
      variant,
    },
  })
}

export { Badge, badgeVariants }
