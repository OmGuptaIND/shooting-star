import React from "react";
import CustomButton from "../custom/CustomButton";

const Title = () => {
	return (
		<div className="flex items-center mb-5">
			<div className="flex-1 text-4xl font-bold">
				<h1
					contentEditable
					suppressContentEditableWarning
					className="text-4xl w-full border-none outline-none font-bold text-white"
				>
					Energy Price Metrics
				</h1>
			</div>

			<div className="flex items-center gap-2">
				<span className="text-custom-gray-secondary cursor-default select-none text-xs font-inter">
					Edited 1h ago
				</span>
				<CustomButton size={"sm"} variant={"custom"}>
					Share
				</CustomButton>
			</div>
		</div>
	);
};

export default React.memo(Title);
